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
	got := hostContext(t, c.fire("SessionStart", map[string]any{"source": "startup"}))
	if took := time.Since(began); took >= time.Second {
		t.Fatalf("the starting hook took %v", took)
	}
	if !strings.Contains(got, "Joined #checkout as @claude1") {
		t.Fatalf("auto-join context %q", got)
	}
	if len(w.dispatcherPIDs()) == 0 || !rosterHas(t, writer, "claude1") {
		t.Fatal("the hall is not running with the session joined")
	}
	if log := readLog(t, w); !strings.Contains(log, "hook claude SessionStart: started the dispatcher (") {
		t.Fatalf("dispatcher.log does not record the start:\n%s", log)
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
