// Package testreap keeps fixture dispatchers from outliving the tests that
// start them. Only tests import it.
//
// Arm gives the test process a random token in CROSSHALL_TESTREAP, which
// every dispatcher launch passes on. A dispatcher belongs to this run when
// its environment holds that exact entry. The token rules out an accidental
// match with a parallel run of the same binary or a process that later
// reuses a pid; a process crafted to carry it is out of scope. Each pid is
// checked again right before every signal.
package testreap

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Key is the environment variable that carries the token.
const Key = "CROSSHALL_TESTREAP"

var (
	arm   sync.Once
	token string
	err   error
	// The watchdog's pipe stays open, and reachable, until this process
	// exits: a collected *os.File would close and set the watchdog off.
	pipe *os.File
)

// The watchdog waits for EOF on its stdin, a pipe only the test process
// holds, so it wakes when that process exits, however it exits. For ten
// seconds it then ends, once a second, each dispatcher whose environment
// holds the entry $1: a vendor session the test left can restart one from a
// hook. It uses the matcher of owned and mine below.
const watchdog = `cat > /dev/null
entry=$1
# mine <pid>: the process is a dispatcher with the exact entry.
mine() {
  if [ -r /proc/self/environ ]; then
    # Newlines inside values become \001, NULs become line ends.
    case "$(tr '\n\0' '\001\n' < "/proc/$1/cmdline" 2>/dev/null | tail -n 1)" in
    __dispatch | __test_dispatch_guardian) ;;
    *) return 1 ;;
    esac
    tr '\n\0' '\001\n' < "/proc/$1/environ" 2>/dev/null | grep -Fxq "$entry"
  else
    # The arguments alone, then the environment, which ps -E prints after
    # them with a newline in a value escaped.
    args=$(ps -ww -p "$1" -o stat= -o args= 2>/dev/null) || return 1
    case "$args" in Z*) return 1 ;; esac
    case "$args" in *" __dispatch" | *" __test_dispatch_guardian") ;; *) return 1 ;; esac
    line=$(ps -E -ww -p "$1" -o command= 2>/dev/null) || return 1
    case " $line " in *" $entry "*) return 0 ;; esac
    return 1
  fi
}
i=0
while [ "$i" -lt 10 ]; do
  if [ -r /proc/self/environ ]; then
    candidates=$(cd /proc && ls -d [0-9]*)
  else
    candidates=$(ps -E -ww -ax -o pid= -o command= | grep -F " $entry" | awk '{print $1}')
  fi
  for pid in $candidates; do
    if mine "$pid"; then kill -TERM "$pid" 2>/dev/null; fi
  done
  i=$((i + 1))
  sleep 1
done`

// Arm sets this run's token in the test process's environment and starts
// its watchdog: when this test process exits, however it exits, the
// dispatchers that carry the token are ended, and Check reports any still
// running after the tests. A fixture that builds a child environment from
// scratch adds Env() to it. The test still stops its own on t.Cleanup.
func Arm(t testing.TB) {
	t.Helper()
	arm.Do(func() {
		raw := make([]byte, 16)
		if _, err = rand.Read(raw); err != nil {
			return
		}
		var r *os.File
		if r, pipe, err = os.Pipe(); err != nil {
			return
		}
		defer r.Close()
		value := hex.EncodeToString(raw)
		watch := exec.Command("/bin/sh", "-c", watchdog, "testreap", Key+"="+value)
		watch.Stdin = r
		// Its own process group: an interrupt to the test's group must not
		// end the watchdog before the dispatchers.
		watch.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		if err = watch.Start(); err != nil {
			return
		}
		go watch.Wait()
		if err = os.Setenv(Key, value); err == nil {
			token = value
		}
	})
	if err != nil {
		t.Fatal("testreap:", err)
	}
}

// Env is the environment entry every dispatcher of this run carries.
func Env() string { return Key + "=" + token }

// Check returns code, or 1 when a dispatcher of this run still runs after
// the tests, or when it cannot list processes. It names and ends each one.
// Call it in TestMain with the result of m.Run.
func Check(code int) int {
	if token == "" {
		return code
	}
	// A dispatcher a cleanup just signalled may still be exiting.
	var leaked []int
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		var err error
		if leaked, err = owned(Env()); err != nil {
			fmt.Fprintln(os.Stderr, "testreap: cannot list processes:", err)
			return 1
		}
		if len(leaked) == 0 || !time.Now().Before(deadline) {
			break
		}
	}
	for _, pid := range leaked {
		fmt.Fprintf(os.Stderr, "testreap: a test left dispatcher %d running\n", pid)
		end(pid, Env())
	}
	if len(leaked) > 0 && code == 0 {
		return 1
	}
	return code
}

// owned lists the running dispatchers whose environment holds entry.
func owned(entry string) ([]int, error) {
	var pids []int
	if runtime.GOOS == "linux" {
		var err error
		pids, err = linuxPIDs()
		if err != nil {
			return nil, err
		}
	} else {
		// One ps lists the candidates; each is checked again on its own.
		out, err := exec.Command("ps", "-E", "-ww", "-ax", "-o", "pid=", "-o", "stat=", "-o", "command=").Output()
		if err != nil {
			return nil, err
		}
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			pidField, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
			pid, err := strconv.Atoi(pidField)
			if err != nil {
				return nil, fmt.Errorf("ps printed %q", line)
			}
			if strings.Contains(rest, entry) { // mine decides
				pids = append(pids, pid)
			}
		}
	}
	var found []int
	for _, pid := range pids {
		ok, err := mine(pid, entry)
		if err != nil {
			return nil, err
		}
		if ok {
			found = append(found, pid)
		}
	}
	return found, nil
}

// mine reports whether pid is a dispatcher whose environment holds entry
// exactly. A process that has ended, or a zombie, is not.
func mine(pid int, entry string) (bool, error) {
	if runtime.GOOS == "linux" {
		dir := filepath.Join("/proc", strconv.Itoa(pid))
		cmdline, err := os.ReadFile(filepath.Join(dir, "cmdline"))
		if err != nil {
			return false, nil
		}
		args := bytes.Split(bytes.TrimSuffix(cmdline, []byte{0}), []byte{0})
		if last := string(args[len(args)-1]); last != "__dispatch" && last != "__test_dispatch_guardian" {
			return false, nil
		}
		return linuxHasEntry(pid, entry), nil
	}
	// The arguments alone, then the environment, which ps -E prints after
	// them with a newline in a value escaped, so entries are space separated.
	args, err := psLine(pid, "-o", "stat=", "-o", "args=")
	if err != nil || args == "" || strings.HasPrefix(args, "Z") ||
		!(strings.HasSuffix(args, " __dispatch") || strings.HasSuffix(args, " __test_dispatch_guardian")) {
		return false, err
	}
	line, err := psLine(pid, "-E", "-o", "command=")
	return err == nil && strings.Contains(" "+line+" ", " "+entry+" "), err
}

// psLine is ps's line for pid, or "" when pid has ended; a failing ps is an
// error.
func psLine(pid int, args ...string) (string, error) {
	out, err := exec.Command("ps", append([]string{"-ww", "-p", strconv.Itoa(pid)}, args...)...).Output()
	if err != nil {
		if syscall.Kill(pid, 0) == syscall.ESRCH {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// EndFixture stops all processes carrying this fixture's exact token,
// including the vendor sessions and their tools. The caller must fence new
// launches before calling it. The token is distinct from every other run's.
func EndFixture(entry string) error {
	kill := killDarwinFixtureProcesses
	if runtime.GOOS == "linux" {
		kill = killLinuxFixtureProcesses
	}
	deadline := time.Now().Add(5 * time.Second)
	empty := false
	for time.Now().Before(deadline) {
		found, err := kill(entry)
		if err != nil {
			return err
		}
		if !found && empty {
			return nil
		}
		empty = !found
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("testreap: fixture processes did not exit")
}

func killDarwinFixtureProcesses(entry string) (bool, error) {
	out, err := exec.Command("/bin/ps", "-E", "-ww", "-ax", "-o", "pid=", "-o", "stat=", "-o", "command=").Output()
	if err != nil {
		return false, err
	}
	found := false
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 || strings.HasPrefix(fields[1], "Z") || !slices.Contains(fields[2:], entry) {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil || pid <= 1 || pid == os.Getpid() {
			return false, fmt.Errorf("testreap: unsafe fixture PID %q", fields[0])
		}
		current, err := psLine(pid, "-E", "-o", "stat=", "-o", "command=")
		if err != nil {
			return false, err
		}
		fields = strings.Fields(current)
		if len(fields) < 2 || strings.HasPrefix(fields[0], "Z") || !slices.Contains(fields[1:], entry) {
			continue
		}
		found = true
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return false, err
		}
	}
	return found, nil
}

func linuxPIDs() ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var pids []int
	for _, e := range entries {
		if pid, err := strconv.Atoi(e.Name()); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}

func linuxHasEntry(pid int, entry string) bool {
	environ, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "environ"))
	return err == nil && slices.ContainsFunc(bytes.Split(environ, []byte{0}), func(variable []byte) bool {
		return string(variable) == entry
	})
}

func killLinuxFixtureProcesses(entry string) (bool, error) {
	pids, err := linuxPIDs()
	if err != nil {
		return false, err
	}
	found := false
	for _, pid := range pids {
		if !linuxHasEntry(pid, entry) {
			continue
		}
		if pid <= 1 || pid == os.Getpid() {
			return false, fmt.Errorf("testreap: unsafe fixture PID %d", pid)
		}
		process, err := os.FindProcess(pid)
		if err != nil {
			return false, err
		}
		if linuxHasEntry(pid, entry) {
			found = true
			err = process.Kill()
		}
		_ = process.Release()
		if err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
			return false, err
		}
	}
	return found, nil
}

// end signals pid while it is still a dispatcher with entry: TERM, then
// KILL after five seconds.
func end(pid int, entry string) {
	still := func() bool {
		ok, err := mine(pid, entry)
		return err == nil && ok
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
