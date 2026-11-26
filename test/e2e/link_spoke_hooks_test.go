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

// An adapter policy changed while a device is connected (#400, 12k, the
// link rule): a device that negotiated the policy capability caches it; a
// device of the base build never receives the event, and its stream goes
// on without a reconnect.
func TestSpokeLinkKeepsItsStreamAcrossAPolicyToggle(t *testing.T) {
	withLinkBuildPairs(t, func(t *testing.T, f *linkFixture) {
		home, spoke := f.home.owner(), f.spoke.owner()
		home.must("owner", "start")
		spoke.must("owner", "start")
		home.must("owner", "channel", "create", "policy-toggle")
		spoke.must("link", linkFixtureTarget, "--name", "fixture-spoke", "--yes")
		waitSpokeLink(t, spoke, "connected")
		homeDB := f.home.db()
		spokeDB := spokeStore(t, f.spoke.state)
		value := func(db *sql.DB, query string, args ...any) (v sql.NullString) {
			_ = db.QueryRow(query, args...).Scan(&v)
			return v
		}
		eventually(t, "the device subscribed", func() bool {
			return value(homeDB, `SELECT stream FROM link_event_stream WHERE closed_at_ms IS NULL`).Valid
		})
		stream := value(homeDB, `SELECT stream FROM link_event_stream WHERE closed_at_ms IS NULL`).String
		head := func() int64 {
			n, _ := strconv.ParseInt(value(homeDB, `SELECT head FROM link_event_cursor`).String, 10, 64)
			return n
		}
		before := head()
		epochBefore := value(homeDB, `SELECT epoch FROM adapter_policy WHERE adapter = 'claude'`).String
		for _, enabled := range []string{"on", "off", "on"} {
			home.must("owner", "adapter", "set", "claude", "--enabled", enabled)
		}
		appended := head() - before
		// A later event still reaches the device and is acknowledged.
		member := f.spoke.member()
		member.must("join", "policy-toggle", "--as", "remote")
		eventually(t, "the device acknowledged every event", func() bool {
			return value(homeDB, `SELECT head = acked AND head > 0 FROM link_event_cursor`).String == "1"
		})
		if got := value(homeDB, `SELECT stream FROM link_event_stream WHERE closed_at_ms IS NULL`); got.String != stream {
			t.Fatalf("the stream reconnected: %q, was %q", got.String, stream)
		}
		switch {
		case strings.HasSuffix(t.Name(), "head-head"):
			epoch := value(homeDB, `SELECT epoch FROM adapter_policy WHERE adapter = 'claude'`).String
			if epoch == epochBefore {
				t.Fatalf("the toggles kept epoch %s", epoch)
			}
			eventually(t, "the device holds the hall's claude policy", func() bool {
				return value(spokeDB, `SELECT epoch FROM spoke_policy WHERE adapter = 'claude' AND enabled = 1`).String == epoch
			})
			if appended != 3 {
				t.Fatalf("%d events for three policy changes, want 3", appended)
			}
		case strings.HasSuffix(t.Name(), "base-spoke"):
			if appended != 0 || value(homeDB, `SELECT policy_capable FROM link_event_stream`).String != "0" {
				t.Fatalf("%d events for a device without the capability", appended)
			}
		}
	})
}
