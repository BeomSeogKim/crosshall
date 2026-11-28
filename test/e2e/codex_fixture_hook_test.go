//go:build e2e

package e2e

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// codexFixtureHookDone is the line the managed Codex fixture prints after
// each hook exits (testdata/linked_codex_daemon).
const codexFixtureHookDone = "fixture hook done"

// codexFixtureHooks runs hooks through the managed Codex fixture.
type codexFixtureHooks struct {
	input      io.Writer
	log        string // the fixture's output
	rendezvous string // the dispatcher's spool root
}

// deliver runs hook and returns once a dispatcher has recorded it, which
// seq reports by moving past its value before the hook ran. A hook the
// dispatcher drains after its deadline is dropped by design, as a busy
// machine can make happen, and nothing else records it: once no packet
// waits and none was recorded, the session runs the hook again.
func (h codexFixtureHooks) deliver(t *testing.T, hook map[string]any, seq func() int64) {
	t.Helper()
	const attempts = 5
	for attempt := 1; ; attempt++ {
		before, ran := seq(), h.hooksRun(t)
		if err := json.NewEncoder(h.input).Encode(hook); err != nil {
			t.Fatal(err)
		}
		eventually(t, "the managed Codex fixture ran the hook", func() bool { return h.hooksRun(t) > ran })
		recorded := false
		eventually(t, "the dispatcher recorded or dropped the hook", func() bool {
			recorded = seq() > before
			return recorded || !h.queued(t, hook)
		})
		if recorded || seq() > before {
			return
		}
		if attempt == attempts {
			t.Fatalf("the dispatcher dropped %v %d times", hook["hook_event_name"], attempts)
		}
		t.Logf("the dispatcher dropped %v after its deadline; the session runs it again", hook["hook_event_name"])
	}
}

func (h codexFixtureHooks) hooksRun(t *testing.T) int {
	t.Helper()
	out, err := os.ReadFile(h.log)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Count(out, []byte(codexFixtureHookDone+"\n"))
}

// queued reports whether the member spool of hook's session holds a packet
// the dispatcher has not finished: it removes one only after recording what
// it did.
func (h codexFixtureHooks) queued(t *testing.T, hook map[string]any) bool {
	t.Helper()
	session, _ := hook["session_id"].(string)
	sum := sha256.Sum256([]byte("codex\x00" + session))
	leaf := filepath.Join(h.rendezvous, "m", hex.EncodeToString(sum[:])[:16])
	if _, err := os.Stat(leaf); err != nil {
		t.Fatalf("member spool of %s: %v", session, err) // a wrong path would read as drained
	}
	return len(fixtureQueuedPackets(t, leaf)) > 0
}
