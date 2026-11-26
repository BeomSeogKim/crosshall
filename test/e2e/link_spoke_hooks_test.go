//go:build e2e

package e2e

import (
	"database/sql"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A Claude session on a linked device runs its hooks against the device's
// cache (#400, 12k): the home's adapter policy reaches the device, the
// hook's facts reach the home with their tokens, a note from the home is
// offered at the next prompt, and the hook's written acknowledgement
// confirms the offer at the home.
func TestSpokeHooksReachTheHome(t *testing.T) {
	f := newLinkFixture(t, binary, binary)
	home, spoke := f.home.owner(), f.spoke.owner()
	home.must("owner", "start")
	spoke.must("owner", "start")
	home.must("owner", "channel", "create", "checkout")
	home.must("owner", "adapter", "set", "claude", "--enabled", "on")
	spoke.must("link", linkFixtureTarget, "--name", "fixture-spoke", "--yes")
	waitSpokeLink(t, spoke, "connected")

	repo := filepath.Join(f.spoke.home, "checkout")
	if err := os.MkdirAll(repo, 0700); err != nil {
		t.Fatal(err)
	}
	c := claudeSession{w: f.spoke, key: "linked-claude-session", pid: strconv.Itoa(os.Getpid()), repo: repo}
	session := &participant{w: f.spoke, extra: []string{"CLAUDE_CODE_SESSION_ID=" + c.key, "CLAUDE_PID=" + c.pid}}
	writer := f.home.member()
	writer.must("join", "checkout", "--as", "writer")

	spokeDB := spokeStore(t, f.spoke.state)
	homeDB := f.home.db()
	value := func(db *sql.DB, query string, args ...any) int64 {
		var v sql.NullInt64
		_ = db.QueryRow(query, args...).Scan(&v)
		return v.Int64
	}
	applied := func() int64 {
		return value(homeDB, `SELECT COUNT(*) FROM link_event_input WHERE kind = 'fact' AND status = 'applied'`)
	}
	eventually(t, "the device holds the hall's claude policy", func() bool {
		return value(spokeDB, `SELECT p.enabled * c.eligible FROM spoke_policy p, spoke_event_cursor c WHERE p.adapter = 'claude'`) == 1
	})

	session.must("join", "checkout", "--as", "remote")
	// Fired right after the join's answer, before the home's event for
	// it: the cut is where the stamp changed, so the start counts.
	c.fire("SessionStart", map[string]any{"source": "startup"})
	eventually(t, "the home installed the join's cut and recorded the start", func() bool {
		return value(homeDB, `SELECT COUNT(*) FROM link_event_cut WHERE key = ? AND absent = 0`, c.key) == 1 && applied() == 1
	})
	id := num(t, writer.must("note", "@remote", "from the home"), "id")
	want := "[crosshall] pending: " + strconv.FormatInt(id, 10)
	eventually(t, "the device cached the offer", func() bool {
		return value(spokeDB, `SELECT COUNT(*) FROM spoke_offer WHERE suppressed = 0`) == 1
	})
	var got string
	deadline := time.Now().Add(fixtureEventTimeout)
	for got == "" && time.Now().Before(deadline) {
		if out := c.fire("UserPromptSubmit", map[string]any{"prompt_id": "p1", "prompt": "synthetic"}); out != "" {
			got = hostContext(t, out)
		}
	}
	if got != want {
		t.Fatalf("the prompt printed %q, want %q", got, want)
	}
	eventually(t, "the home recorded the prompt and the written offer", func() bool {
		return applied() >= 2 &&
			value(homeDB, `SELECT COUNT(*) FROM link_event_input WHERE kind = 'written' AND status = 'applied'`) == 1
	})
	eventually(t, "the device's hook outbox drained", func() bool {
		return value(spokeDB, `SELECT COUNT(*) FROM spoke_hook_outbox`) == 0
	})
	// The same mail is not offered again.
	if out := c.fire("UserPromptSubmit", map[string]any{"prompt_id": "p2"}); strings.Contains(out, "pending") {
		t.Fatalf("a written offer was shown again: %q", out)
	}
	// A prompt fired right after a move counts too.
	home.must("owner", "channel", "create", "other")
	before := applied()
	session.must("join", "other", "--move")
	c.fire("UserPromptSubmit", map[string]any{"prompt_id": "p3"})
	eventually(t, "the home recorded the prompt after the move", func() bool { return applied() == before+1 })
	if n := value(homeDB, `SELECT COUNT(*) FROM link_event_input WHERE status = 'stale'`); n != 0 {
		t.Fatalf("%d hook facts were recorded stale", n)
	}
}
