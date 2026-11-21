//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"
)

func assertWaitFailure(t *testing.T, child <-chan waitExit, status int, code string, changed time.Time) {
	t.Helper()
	select {
	case result := <-child:
		var failure struct {
			Code string `json:"code"`
		}
		processErr, _ := result.err.(*exec.ExitError)
		if err := json.Unmarshal([]byte(result.stdout), &failure); err != nil ||
			processErr == nil || processErr.ExitCode() != status || failure.Code != code {
			t.Fatalf("wait after %v: %v, stdout %s, stderr %s; want exit %d (%s)",
				time.Since(changed), result.err, result.stdout, result.stderr, status, code)
		}
		t.Logf("wait after %v: exit %d, %s", time.Since(changed), status, result.stdout)
	case <-time.After(fixtureEventTimeout):
		t.Fatal("wait did not report the change")
	}
}

func pauseFixtureProcess(t *testing.T, process *os.Process) {
	t.Helper()
	t.Cleanup(func() { _ = process.Signal(syscall.SIGCONT) })
	if err := process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
}

func resumeFixtureProcess(t *testing.T, process *os.Process) {
	t.Helper()
	if err := process.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
}

func TestWaitFailsWhenDispatcherStops(t *testing.T) {
	for _, mode := range []struct {
		name string
		args []string
	}{
		{"untimed", nil},
		{"timed", []string{"--existing", "--work-only", "--timeout", "900ms"}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			for _, stop := range []string{"graceful", "killed"} {
				t.Run(stop, func(t *testing.T) {
					w, owner, members := setup(t)
					child := startWaitAfterInitialQuery(t, members[0], mode.args...)
					changed := time.Now()
					if stop == "graceful" {
						owner.must("owner", "stop")
					} else {
						w.kill()
					}
					assertWaitFailure(t, child, 5, "dispatcher_unavailable", changed)
				})
			}
		})
	}
}

func TestWaitRetriesSlowDispatcherPoll(t *testing.T) {
	for _, mode := range []struct {
		name string
		args []string
	}{
		{"untimed", []string{"--existing"}},
		{"timed", []string{"--existing", "--timeout", "7s"}},
	} {
		t.Run(mode.name, func(t *testing.T) {
			w, _, members := setup(t)
			form, tax := members[0], members[1]
			pids := w.dispatcherPIDs()
			if len(pids) != 1 {
				t.Fatalf("want one fixture dispatcher, got %v", pids)
			}
			dispatcher, err := os.FindProcess(pids[0])
			if err != nil {
				t.Fatal(err)
			}
			child := startWaitAfterInitialQuery(t, form, mode.args...)
			pauseFixtureProcess(t, dispatcher)
			requireWaiting(t, child, 1600*time.Millisecond)
			resumeFixtureProcess(t, dispatcher)
			message := num(t, tax.must("note", "@form", "sent after the slow poll"), "id")
			assertWorkItem(t, awaitWorkWait(t, child, "arrived").Unread, message, "note")
		})
	}
}

func TestWaitDeliversAcrossDispatcherRestart(t *testing.T) {
	_, owner, members := setup(t)
	form, tax := members[0], members[1]
	process, child := startWaitProcessAfterInitialQuery(t, form, "--existing", "--timeout", "8s")
	pauseFixtureProcess(t, process)
	owner.must("owner", "stop")
	tax.must("who")
	message := num(t, tax.must("note", "@form", "sent after the restart"), "id")
	resumeFixtureProcess(t, process)
	assertWorkItem(t, awaitWorkWait(t, child, "arrived").Unread, message, "note")
}

func TestWaitDeliversAcrossLeaveAndRejoin(t *testing.T) {
	_, _, members := setup(t)
	form, tax := members[0], members[1]
	process, child := startWaitProcessAfterInitialQuery(t, form)
	pauseFixtureProcess(t, process)
	form.must("leave")
	form.must("join", "checkout", "--as", "form")
	message := num(t, tax.must("note", "@form", "sent after rejoining"), "id")
	resumeFixtureProcess(t, process)
	assertWorkItem(t, awaitWorkWait(t, child, "").Unread, message, "note")
}

func TestWaitBurstLeavesAllMessagesInInbox(t *testing.T) {
	_, _, members := setup(t)
	form, tax := members[0], members[1]
	process, child := startWaitProcessAfterInitialQuery(t, form, "--existing")
	pauseFixtureProcess(t, process)
	const count = 8
	var results [count]outcome
	var sent sync.WaitGroup
	for i := range count {
		sent.Go(func() {
			results[i] = tax.run("note", "@form", fmt.Sprintf("burst note %d", i))
		})
	}
	sent.Wait()
	var want []int64
	for _, out := range results {
		if out.exit != 0 || out.obj == nil {
			t.Fatalf("sending burst note: exit %d, %s", out.exit, out.stdout)
		}
		want = append(want, num(t, out, "id"))
	}
	slices.Sort(want)
	resumeFixtureProcess(t, process)
	mail := awaitWorkWait(t, child, "arrived")
	var seen []int64
	for _, row := range mail.Unread {
		seen = append(seen, row.ID)
	}
	if !slices.Equal(seen, want) {
		t.Fatalf("wait returned %v, want %v", seen, want)
	}
	inbox := form.must("inbox")
	if got := ids(inbox.obj["unread"]); !slices.Equal(got, want) {
		t.Fatalf("inbox returned %v, want %v", got, want)
	}
}
