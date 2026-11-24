// Package testreap keeps fixture dispatchers from outliving the tests that
// start them. Only tests import it.
package testreap

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

var (
	mu      sync.Mutex
	watched []string
)

// The watchdog reads its stdin until the test process exits, which closes
// the pipe even when a timeout or a kill skips t.Cleanup. It then ends every
// dispatcher run from the watched binary, for a while, because a vendor
// session the test left can still start one from a hook.
const watchdog = `read -r _
i=0
while [ "$i" -lt 10 ]; do
  ps -ww -axo pid=,args= | while read -r pid args; do
    for exe in "$@"; do
      case "$args" in
      "$exe __dispatch"|"$exe __test_dispatch_guardian") kill -TERM "$pid" 2>/dev/null ;;
      esac
    done
  done
  i=$((i + 1))
  sleep 1
done`

// Arm watches the crosshall binary at exe: when this test process exits,
// however it exits, the dispatchers it runs are ended, and Check reports any
// still running after the tests. The test still stops its own on t.Cleanup.
func Arm(t testing.TB, exe string) {
	t.Helper()
	// A dispatcher may name the binary by its resolved path.
	paths := []string{exe}
	if resolved, err := filepath.EvalSymlinks(filepath.Dir(exe)); err == nil && resolved != filepath.Dir(exe) {
		paths = append(paths, filepath.Join(resolved, filepath.Base(exe)))
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	watch := exec.Command("/bin/sh", append([]string{"-c", watchdog, "testreap"}, paths...)...)
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
	mu.Lock()
	watched = append(watched, paths...)
	mu.Unlock()
	t.Cleanup(func() {
		_ = watch.Process.Kill()
		_ = watch.Wait()
		w.Close()
	})
}

// Check returns code, or 1 when a dispatcher of a binary Arm watched, or of
// this test binary, still runs after the tests. It names and ends each one.
// Call it in TestMain with the result of m.Run.
func Check(code int) int {
	mu.Lock()
	binaries := append([]string(nil), watched...)
	mu.Unlock()
	if exe, err := os.Executable(); err == nil {
		binaries = append(binaries, exe)
	}
	// A dispatcher a cleanup just signalled may still be exiting.
	var leaked map[int]string
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		leaked = dispatchers(binaries)
		if len(leaked) == 0 || !time.Now().Before(deadline) {
			break
		}
	}
	for pid, args := range leaked {
		fmt.Fprintf(os.Stderr, "testreap: a test left dispatcher %d running: %s\n", pid, args)
		end(pid)
	}
	if len(leaked) > 0 && code == 0 {
		return 1
	}
	return code
}

// dispatchers maps the pid of each live dispatcher run from one of binaries
// to its command line.
func dispatchers(binaries []string) map[int]string {
	out, _ := exec.Command("ps", "-ww", "-axo", "pid=,stat=,args=").Output()
	found := map[int]string{}
	for _, line := range strings.Split(string(out), "\n") {
		pidText, rest, _ := strings.Cut(strings.TrimSpace(line), " ")
		stat, args, _ := strings.Cut(strings.TrimSpace(rest), " ")
		pid, err := strconv.Atoi(pidText)
		if err != nil || pid == os.Getpid() || strings.HasPrefix(stat, "Z") {
			continue
		}
		args = strings.TrimSpace(args)
		for _, exe := range binaries {
			if args == exe+" __dispatch" || args == exe+" __test_dispatch_guardian" {
				found[pid] = args
			}
		}
	}
	return found
}

func end(pid int) {
	_ = syscall.Kill(pid, syscall.SIGTERM)
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
