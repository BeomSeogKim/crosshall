// Package testreap keeps fixture dispatchers from outliving the tests that
// start them. Only tests import it.
//
// A dispatcher belongs to a test when its environment, set when it was
// launched, names that test's own state root in CROSSHALL_STATE_DIR. Every
// launch path passes it, including a dispatcher a vendor hook restarts. A
// parallel run of the same binary, or a process that later reuses the pid,
// has another state root, and testreap reads the environment again before
// each signal.
package testreap

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const stateEnv = "CROSSHALL_STATE_DIR="

var (
	mu    sync.Mutex
	roots []string // every armed state root, as given and resolved
	// The watchdogs' pipes stay open, and reachable, until this process
	// exits: a collected *os.File would close and set its watchdog off.
	pipes []*os.File
)

// The watchdog waits for EOF on its stdin, a pipe only the test process
// holds, so it wakes when that process exits, however it exits. For ten
// seconds it then ends, once a second, each dispatcher whose environment
// names one of its state roots ($@): a vendor session the test left can
// restart one from a hook. Linux reads /proc; macOS reads ps -E.
const watchdog = `cat > /dev/null
owned() {
  if [ -r /proc/self/environ ]; then
    for d in /proc/[0-9]*; do
      case "$(tr '\0' ' ' < "$d/cmdline" 2>/dev/null)" in
      *" __dispatch " | *" __test_dispatch_guardian ") ;;
      *) continue ;;
      esac
      for root in "$@"; do
        if tr '\0' '\n' < "$d/environ" 2>/dev/null | grep -Fxq "CROSSHALL_STATE_DIR=$root"; then
          echo "${d#/proc/}"
          break
        fi
      done
    done
  else
    ps -E -ww -ax -o pid= -o stat= -o command= | while read -r pid stat rest; do
      case "$stat" in Z*) continue ;; esac
      case " $rest " in
      *" __dispatch "* | *" __test_dispatch_guardian "*) ;;
      *) continue ;;
      esac
      for root in "$@"; do
        case " $rest " in *" CROSSHALL_STATE_DIR=$root "*) echo "$pid"; break ;; esac
      done
    done
  fi
}
i=0
while [ "$i" -lt 10 ]; do
  for pid in $(owned "$@"); do kill -TERM "$pid" 2>/dev/null; done
  i=$((i + 1))
  sleep 1
done`

// Arm watches the dispatchers launched for stateRoot, a state root only this
// test uses: when this test process exits, however it exits, they are ended,
// and Check reports any still running after the tests. The test still stops
// its own on t.Cleanup.
func Arm(t testing.TB, stateRoot string) {
	t.Helper()
	armed := []string{stateRoot}
	if resolved := resolve(stateRoot); resolved != stateRoot {
		armed = append(armed, resolved)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	watch := exec.Command("/bin/sh", append([]string{"-c", watchdog, "testreap"}, armed...)...)
	watch.Stdin = r
	// Its own process group: an interrupt to the test's group must not end
	// the watchdog before the dispatchers.
	watch.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := watch.Start(); err != nil {
		r.Close()
		w.Close()
		t.Fatal(err)
	}
	r.Close()
	go watch.Wait()
	mu.Lock()
	roots = append(roots, armed...)
	pipes = append(pipes, w)
	mu.Unlock()
}

// Check returns code, or 1 when a dispatcher of an armed state root still
// runs after the tests, or when it cannot list processes. It names and ends
// each one. Call it in TestMain with the result of m.Run.
func Check(code int) int {
	mu.Lock()
	armed := append([]string(nil), roots...)
	mu.Unlock()
	if len(armed) == 0 {
		return code
	}
	// A dispatcher a cleanup just signalled may still be exiting.
	var leaked []proc
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		var err error
		if leaked, err = owned(armed); err != nil {
			fmt.Fprintln(os.Stderr, "testreap: cannot list processes:", err)
			return 1
		}
		if len(leaked) == 0 || !time.Now().Before(deadline) {
			break
		}
	}
	for _, p := range leaked {
		fmt.Fprintf(os.Stderr, "testreap: a test left dispatcher %d of %s running\n", p.pid, p.root)
		end(p.pid, armed)
	}
	if len(leaked) > 0 && code == 0 {
		return 1
	}
	return code
}

type proc struct {
	pid  int
	root string
}

// owned lists the running dispatchers whose environment names one of roots.
func owned(roots []string) ([]proc, error) {
	if runtime.GOOS == "linux" {
		return ownedProc(roots)
	}
	out, err := exec.Command("ps", "-E", "-ww", "-ax", "-o", "pid=", "-o", "stat=", "-o", "command=").Output()
	if err != nil {
		return nil, err
	}
	var found []proc
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || strings.HasPrefix(fields[1], "Z") {
			continue
		}
		rest := " " + strings.Join(fields[2:], " ") + " "
		if !strings.Contains(rest, " __dispatch ") && !strings.Contains(rest, " __test_dispatch_guardian ") {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			return nil, fmt.Errorf("ps printed %q", line)
		}
		for _, root := range roots {
			if strings.Contains(rest, " "+stateEnv+root+" ") {
				found = append(found, proc{pid, root})
				break
			}
		}
	}
	return found, nil
}

func ownedProc(roots []string) ([]proc, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var found []proc
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		// A process that ended meanwhile, or a zombie, has no command line.
		cmdline, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "cmdline"))
		args := bytes.Split(bytes.TrimSuffix(cmdline, []byte{0}), []byte{0})
		if err != nil || len(args) < 2 {
			continue
		}
		if last := string(args[len(args)-1]); last != "__dispatch" && last != "__test_dispatch_guardian" {
			continue
		}
		environ, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "environ"))
		if err != nil {
			continue
		}
		for _, root := range roots {
			if bytes.Contains(append([]byte{0}, environ...), []byte("\x00"+stateEnv+root+"\x00")) {
				found = append(found, proc{pid, root})
				break
			}
		}
	}
	return found, nil
}

// end signals pid while it is still a dispatcher of roots: TERM, then KILL
// after five seconds.
func end(pid int, roots []string) {
	still := func() bool {
		found, err := owned(roots)
		if err != nil {
			return false
		}
		for _, p := range found {
			if p.pid == pid {
				return true
			}
		}
		return false
	}
	if !still() {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGTERM)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		if !still() {
			return
		}
	}
	if still() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
}

// resolve is path with symlinks resolved in its longest existing prefix, as
// a dispatcher that resolves its state root sees it.
func resolve(path string) string {
	dir, rest := path, ""
	for {
		if resolved, err := filepath.EvalSymlinks(dir); err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return path
		}
		dir, rest = parent, filepath.Join(filepath.Base(dir), rest)
	}
}
