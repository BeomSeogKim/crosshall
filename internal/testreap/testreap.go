// Package testreap keeps fixture dispatchers from outliving the tests that
// start them. Only tests import it.
//
// A dispatcher belongs to a test run when the ready marker of that run's own
// rendezvous named it. testreap remembers each such dispatcher by pid and
// start time, and signals a process only while both still match, so a
// parallel run of the same binary and a reused pid are never touched.
package testreap

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

type dispatcher struct {
	pid     int
	started string // ps lstart
}

var (
	mu    sync.Mutex
	ready []string // the ready markers Arm watches
	seen  = map[dispatcher]bool{}
	// The watchdogs' pipes stay open, and reachable, until this process
	// exits: a collected *os.File would close and set its watchdog off.
	pipes []*os.File
)

// The watchdog collects "<pid>|<start time>" lines from its stdin, a pipe only
// the test process holds, until that process exits, however it exits. It then
// ends each collected dispatcher still running with that start time, and for
// ten seconds also the one the ready marker ($1) names, because a vendor
// session the test left can restart one from a hook.
const watchdog = `seen=
while IFS= read -r line; do seen="$seen $line"; done
started() { ps -p "$1" -o lstart= 2>/dev/null | sed 's/^ *//;s/ *$//' | tr ' ' '_'; }
dispatching() {
  case "$(ps -ww -p "$1" -o stat= -o args= 2>/dev/null)" in
  Z*) return 1 ;;
  *" __dispatch"|*" __test_dispatch_guardian") return 0 ;;
  esac
  return 1
}
i=0
while [ "$i" -lt 10 ]; do
  for entry in $seen; do
    pid=${entry%%|*}
    if dispatching "$pid" && [ "$(started "$pid")" = "${entry#*|}" ]; then kill -TERM "$pid"; fi
  done
  pid=$(sed -n 's/.*"pid":\([0-9][0-9]*\).*/\1/p' "$1" 2>/dev/null)
  if [ -n "$pid" ] && dispatching "$pid"; then kill -TERM "$pid"; fi
  i=$((i + 1))
  sleep 1
done`

// Arm watches the dispatcher ready marker at readyFile, in a rendezvous only
// this test run uses: when this test process exits, however it exits, the
// dispatchers that marker named are ended, and Check reports any still
// running after the tests. The test still stops its own on t.Cleanup.
func Arm(t testing.TB, readyFile string) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	watch := exec.Command("/bin/sh", "-c", watchdog, "testreap", readyFile)
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
	ready = append(ready, readyFile)
	pipes = append(pipes, w)
	mu.Unlock()
	// Learn each dispatcher the marker names while the test runs. The pipe
	// stays open until this process exits, so the watchdog never stands
	// down before a later abort.
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go func() {
		tick := time.NewTicker(100 * time.Millisecond)
		defer tick.Stop()
		for {
			if d, ok := named(readyFile); ok {
				mu.Lock()
				fresh := !seen[d]
				seen[d] = true
				mu.Unlock()
				if fresh {
					fmt.Fprintf(w, "%d|%s\n", d.pid, strings.ReplaceAll(d.started, " ", "_"))
				}
			}
			select {
			case <-stop:
				return
			case <-tick.C:
			}
		}
	}()
}

// Check returns code, or 1 when a dispatcher an armed marker named still
// runs after the tests, or when it cannot tell. It names and ends each one.
// Call it in TestMain with the result of m.Run.
func Check(code int) int {
	if _, err := exec.LookPath("ps"); err != nil {
		fmt.Fprintln(os.Stderr, "testreap: cannot list processes:", err)
		return 1
	}
	mu.Lock()
	candidates := make([]dispatcher, 0, len(seen))
	for d := range seen {
		candidates = append(candidates, d)
	}
	files := append([]string(nil), ready...)
	mu.Unlock()
	for _, file := range files {
		if d, ok := named(file); ok {
			candidates = append(candidates, d)
		}
	}
	// A dispatcher a cleanup just signalled may still be exiting.
	var leaked []dispatcher
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		leaked = leaked[:0]
		for _, d := range candidates {
			if running(d) && !contains(leaked, d) {
				leaked = append(leaked, d)
			}
		}
		if len(leaked) == 0 || !time.Now().Before(deadline) {
			break
		}
	}
	for _, d := range leaked {
		args, _ := exec.Command("ps", "-ww", "-p", strconv.Itoa(d.pid), "-o", "args=").Output()
		fmt.Fprintf(os.Stderr, "testreap: a test left dispatcher %d running: %s", d.pid, args)
		end(d)
	}
	if len(leaked) > 0 && code == 0 {
		return 1
	}
	return code
}

// named is the dispatcher the ready marker at file names, if it runs.
func named(file string) (dispatcher, bool) {
	raw, err := os.ReadFile(file)
	var marker struct {
		PID int `json:"pid"`
	}
	if err != nil || json.Unmarshal(raw, &marker) != nil || marker.PID <= 0 {
		return dispatcher{}, false
	}
	started, ok := startTime(marker.PID)
	d := dispatcher{marker.PID, started}
	return d, ok && running(d)
}

// running reports whether d still runs as a dispatcher with its start time.
// A zombie, which nothing may reap, has ended.
func running(d dispatcher) bool {
	out, err := exec.Command("ps", "-ww", "-p", strconv.Itoa(d.pid), "-o", "stat=", "-o", "args=").Output()
	line := strings.TrimSpace(string(out))
	if err != nil || strings.HasPrefix(line, "Z") ||
		!(strings.HasSuffix(line, " __dispatch") || strings.HasSuffix(line, " __test_dispatch_guardian")) {
		return false
	}
	started, ok := startTime(d.pid)
	return ok && started == d.started
}

func startTime(pid int) (string, bool) {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "lstart=").Output()
	started := strings.TrimSpace(string(out))
	return started, err == nil && started != ""
}

func end(d dispatcher) {
	if running(d) {
		_ = syscall.Kill(d.pid, syscall.SIGTERM)
	}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		if !running(d) {
			return
		}
	}
	if running(d) {
		_ = syscall.Kill(d.pid, syscall.SIGKILL)
	}
}

func contains(list []dispatcher, d dispatcher) bool {
	for _, e := range list {
		if e == d {
			return true
		}
	}
	return false
}
