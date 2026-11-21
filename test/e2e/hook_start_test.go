//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// After a reboot the dispatcher is stopped and the rendezvous is gone. The
// first session's start brings the hall back and joins in that callback,
// inside the 1 s hook timeout the manifests install.
func TestHookStartsAStoppedDispatcherAndEnrolls(t *testing.T) {
	w, writer, c := m2World(t)
	w.stop()

	began := time.Now()
	out := c.fire("SessionStart", map[string]any{"source": "startup"})
	if took := time.Since(began); took >= time.Second {
		t.Fatalf("the starting hook took %v", took)
	}
	if out == "" {
		// A loaded machine can miss the start deadline: the hook then says
		// so and leaves the start running, and the next session start joins.
		if log := readLog(t, w); !strings.Contains(log, "hook claude SessionStart: start_pending: ") {
			t.Fatalf("the session start neither joined nor logged start_pending:\n%s", log)
		}
		if len(w.dispatcherPIDs()) == 0 {
			t.Fatal("start_pending without a launched dispatcher")
		}
		w.owner().must("owner", "start") // waits for that dispatcher to answer
		out = c.fire("SessionStart", map[string]any{"source": "startup"})
	} else if log := readLog(t, w); !strings.Contains(log, "hook claude SessionStart: started the dispatcher (") {
		t.Fatalf("dispatcher.log does not record the start:\n%s", log)
	}
	if got := hostContext(t, out); !strings.Contains(got, "Joined #checkout as @claude1") {
		t.Fatalf("auto-join context %q", got)
	}
	if !rosterHas(t, writer, "claude1") {
		t.Fatal("the session is not joined")
	}
}

// An owner stop withdraws the policy, so no hook brings the hall back until
// the owner starts it; the start publishes the policy again.
func TestHookLeavesADispatcherTheOwnerStopped(t *testing.T) {
	w, writer, c := m2World(t)
	if got := c.startWithContext("startup"); !strings.Contains(got, "Joined #checkout as @claude1") || !rosterHas(t, writer, "claude1") {
		t.Fatalf("the initial session did not join: %q", got)
	}
	w.owner().must("owner", "stop")

	for _, fire := range []struct {
		event  string
		fields map[string]any
	}{
		{"Notification", map[string]any{"notification_type": "permission_prompt"}},
		{"UserPromptSubmit", map[string]any{"prompt_id": "p1", "prompt": "synthetic"}},
		{"SessionStart", map[string]any{"source": "startup"}},
	} {
		if got := c.fire(fire.event, fire.fields); got != "" {
			t.Fatalf("%s after an owner stop printed %q", fire.event, got)
		}
	}
	time.Sleep(200 * time.Millisecond) // a spawned dispatcher would hold its lock by now
	if pids := w.dispatcherPIDs(); len(pids) != 0 {
		t.Fatalf("a hook restarted the dispatcher the owner stopped: %v", pids)
	}
	w.owner().must("owner", "start")
	next := c
	next.key, next.pid = "synthetic-next-session", "4343"
	if got := next.startWithContext("startup"); !strings.Contains(got, "Joined #checkout as @claude2") || !rosterHas(t, writer, "claude2") {
		t.Fatalf("the new session did not join after the owner's start: %q", got)
	}
}

func TestHookLeavesTheDispatcherStoppedWhenThePolicyIsOff(t *testing.T) {
	w, _, c := m2World(t)
	w.owner().must("owner", "adapter", "set", "claude", "--enabled", "off")
	w.stop()

	if got := c.fire("SessionStart", map[string]any{"source": "startup"}); got != "" {
		t.Fatalf("a hook with the adapter off printed %q", got)
	}
	c.fire("UserPromptSubmit", map[string]any{"prompt_id": "p1", "prompt": "synthetic"})
	time.Sleep(200 * time.Millisecond) // a spawned dispatcher would hold its lock by now
	if pids := w.dispatcherPIDs(); len(pids) != 0 {
		t.Fatalf("a hook started the dispatcher with the adapter off: %v", pids)
	}
	if log := readLog(t, w); strings.Contains(log, "hook claude") {
		t.Fatalf("dispatcher.log records a hook start:\n%s", log)
	}
}

func readLog(t *testing.T, w *world) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(w.state, "dispatcher.log"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// After a crash an owner stop finds no dispatcher, and still withdraws the
// policy the crash left behind, so hooks keep the hall stopped.
func TestOwnerStopAfterACrashHoldsAgainstHooks(t *testing.T) {
	w, _, c := m2World(t)
	c.fire("SessionStart", map[string]any{"source": "startup"})
	w.kill()
	w.owner().must("owner", "stop")

	if got := c.fire("Notification", map[string]any{"notification_type": "permission_prompt"}); got != "" {
		t.Fatalf("Notification after an owner stop printed %q", got)
	}
	time.Sleep(200 * time.Millisecond) // a spawned dispatcher would hold its lock by now
	if pids := w.dispatcherPIDs(); len(pids) != 0 {
		t.Fatalf("a hook restarted the dispatcher after the owner stop: %v", pids)
	}
	for _, path := range []string{filepath.Join(w.state, "hook-policy"), filepath.Join(w.rendezvous(), "policy")} {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("%s survived the owner stop: %v", path, err)
		}
	}
}
