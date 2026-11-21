//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/ident"
)

// Issue #156: /clear in Claude Code starts a new session ID in the same
// process. The new session continues the member: its alias, its open
// requests and its owed replies.
func TestClearInTheSameProcessKeepsTheMember(t *testing.T) {
	for _, how := range []string{"auto-joined", "resumed outside the repository", "joined by hand while running"} {
		t.Run(how, func(t *testing.T) {
			w, writer, c := m2World(t)
			c.pid = strconv.Itoa(os.Getpid()) // a running process stands for Claude Code
			alias := "claude1"
			switch how {
			case "auto-joined":
				c.fire("SessionStart", map[string]any{"source": "startup"})
			case "resumed outside the repository":
				// Joined by hand outside the default's repository, then resumed:
				// the resumed start binds the process.
				c.repo = w.home
				alias = "worker"
				(&participant{w: w, extra: []string{"CLAUDE_CODE_SESSION_ID=" + c.key}}).must("join", "checkout", "--as", alias)
				c.fire("SessionStart", map[string]any{"source": "resume"})
			default:
				// No start was seen: the join inside the session binds its process,
				// and the session's next prompt reports that launch.
				c.repo = w.home
				alias = "worker"
				(&participant{w: w, extra: []string{"CLAUDE_CODE_SESSION_ID=" + c.key, "CLAUDE_PID=" + c.pid}}).must("join", "checkout", "--as", alias)
				c.fireAndRecord("UserPromptSubmit", "prompt_submitted", map[string]any{"prompt_id": "after-join"})
				var launch string
				if err := w.db().QueryRow(`SELECT launch_ref FROM observation WHERE turn_ref = 'after-join' AND kind = 'prompt_submitted'`).Scan(&launch); err != nil ||
					!strings.HasPrefix(launch, "pid-"+c.pid+"-") {
					t.Fatalf("the prompt after the join reported launch %q (%v), want the joined process", launch, err)
				}
			}
			owed := writer.must("ask", "@"+alias, "owed question")
			(&participant{w: w, extra: []string{"CLAUDE_CODE_SESSION_ID=" + c.key}}).must("read")
			unread := num(t, writer.must("ask", "@"+alias, "unread question"), "id")

			c.fireAndRecord("SessionEnd", "session_ended", map[string]any{"reason": "clear"})
			cleared := claudeSession{w: w, key: c.key + "-cleared", pid: c.pid, repo: c.repo}
			got := hostContext(t, cleared.fire("SessionStart", map[string]any{"source": "clear"}))
			if !strings.Contains(got, "You are @"+alias+" in #checkout") {
				t.Fatalf("the cleared session was not told who it is: %q", got)
			}
			inbox := (&participant{w: w, extra: []string{"CLAUDE_CODE_SESSION_ID=" + cleared.key}}).must("inbox")
			if !strings.Contains(inbox.stdout, strconv.FormatInt(unread, 10)) ||
				!strings.Contains(inbox.stdout, strconv.FormatInt(num(t, owed, "id"), 10)) {
				t.Fatalf("the cleared session does not hold the open requests:\n%s", inbox.stdout)
			}
			if !rosterHas(t, writer, alias) {
				t.Fatalf("the asker sees @%s departed", alias)
			}
			var live int
			if err := w.db().QueryRow(`SELECT COUNT(*) FROM binding WHERE state = 'live'`).Scan(&live); err != nil || live != 2 {
				t.Fatalf("live bindings %d (%v), want the writer and the one member", live, err)
			}

			// The cleared conversation is told where its member went, and may
			// join as a new member (lead decision O1).
			before := &participant{w: w, extra: []string{"CLAUDE_CODE_SESSION_ID=" + c.key}}
			refused := before.run("inbox")
			if refused.exit == 0 || !strings.Contains(refused.stdout+refused.stderr, "cleared into @"+alias+" in #checkout") ||
				!strings.Contains(refused.stdout+refused.stderr, "crosshall join") {
				t.Fatalf("the cleared conversation's inbox: exit %d\n%s%s", refused.exit, refused.stdout, refused.stderr)
			}
			// The conversation's own hooks, also from a first contact after its
			// directory is gone, keep it cleared (Astra #674).
			leafDir := filepath.Join(w.rendezvous(), "m", (ident.Identity{Vendor: "claude", Key: c.key}).Digest()[:16])
			if err := os.RemoveAll(leafDir); err != nil {
				t.Fatal(err)
			}
			if out := c.fire("SessionStart", map[string]any{"source": "resume"}); out != "" {
				t.Fatalf("the cleared conversation's resume printed %q", out)
			}
			c.fire("UserPromptSubmit", map[string]any{"prompt_id": "after-clear"})
			// A failed join keeps the cleared state and its guidance (Astra #672).
			if o := before.run("join", "no-such-channel"); o.exit == 0 {
				t.Fatal("a join to a missing channel succeeded")
			}
			if again := before.run("inbox"); !strings.Contains(again.stdout+again.stderr, "cleared into @"+alias) {
				t.Fatalf("a failed join released the cleared conversation:\n%s%s", again.stdout, again.stderr)
			}
			// The member's roster cursor does not follow the conversation into
			// its new membership (Astra #674): plant one far ahead.
			ack := filepath.Join(leafDir, "ack")
			if err := os.WriteFile(ack, []byte("999999\n"), 0600); err != nil {
				t.Fatal(err)
			}
			// In one case a renderer holds the directory for 4.5 s, past the
			// CLI's wait (Astra #761): the join's receipt is left pending and
			// the next request applies it.
			release := func() {}
			if how == "auto-joined" {
				held, err := os.Open(leafDir)
				if err != nil {
					t.Fatal(err)
				}
				if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX); err != nil {
					t.Fatal(err)
				}
				timer := time.AfterFunc(4500*time.Millisecond, func() { held.Close() })
				release = func() { timer.Stop(); held.Close() }
			}
			joined := before.must("join", "checkout")
			if how == "auto-joined" {
				if pending, _ := filepath.Glob(filepath.Join(leafDir, "pending-*")); len(pending) != 1 {
					t.Fatalf("a join under a held lock left %d pending receipts, want 1", len(pending))
				}
			}
			release()
			if how == "auto-joined" {
				// The dispatcher publishes the new generation on its next pass.
				pending, _ := filepath.Glob(filepath.Join(leafDir, "pending-*"))
				want := strings.TrimPrefix(filepath.Base(pending[0]), "pending-")
				for deadline := time.Now().Add(3 * time.Second); ; time.Sleep(50 * time.Millisecond) {
					if gen, _ := os.ReadFile(filepath.Join(leafDir, "gen")); strings.TrimSpace(string(gen)) == want {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("the join's generation was not published after the lock was released")
					}
				}
			}
			if raw, err := os.ReadFile(ack); err == nil && strings.TrimSpace(string(raw)) == "999999" {
				t.Fatal("the cleared member's roster cursor survived the new join")
			}
			if got := joined.obj["alias"]; got == alias || got == "" || got == nil {
				t.Fatalf("the cleared conversation joined as %v, want a new member", got)
			}
			if !rosterHas(t, writer, alias) {
				t.Fatalf("the new member took @%s away", alias)
			}
			// Its join reply was acknowledged under the new generation (Astra
			// #708): a later member is announced, and a repeat join is short.
			w.member().must("join", "checkout", "--as", "later")
			if who := before.mustPlain("who"); !strings.Contains(who, "@later") {
				t.Fatalf("the new member was not told about @later:\n%s", who)
			}
			if again := before.mustPlain("join", "checkout"); !strings.Contains(again, "Already in") {
				t.Fatalf("a repeat join did not say Already in:\n%s", again)
			}
		})
	}
}

// Issue #164: a command of the replaced conversation still rendering when
// /clear takes the member over records what it showed in its own directory
// after the takeover read it. The member's next command applies it, so the
// rules and the arrival it showed are not shown again.
func TestAReceiptRecordedAfterAClearIsKept(t *testing.T) {
	w, _, c := m2World(t)
	c.pid = strconv.Itoa(os.Getpid())
	c.fire("SessionStart", map[string]any{"source": "startup"})
	w.member().must("join", "checkout", "--as", "latepeer")
	cleared := claudeSession{w: w, key: c.key + "-cleared", pid: c.pid, repo: c.repo}
	if got := hostContext(t, cleared.fire("SessionStart", map[string]any{"source": "clear"})); !strings.Contains(got, "You are @claude1 in #checkout") {
		t.Fatalf("the clear did not keep the member: %q", got)
	}
	// The old conversation's command finishes rendering now: it showed the
	// rules and the roster up to the latest change.
	var latest int64
	if err := w.db().QueryRow(`SELECT MAX(seq) FROM roster_change`).Scan(&latest); err != nil {
		t.Fatal(err)
	}
	oldLeaf := filepath.Join(w.rendezvous(), "m", (ident.Identity{Vendor: "claude", Key: c.key}).Digest()[:16])
	for _, receipt := range []string{"ack", "rules"} {
		if err := os.WriteFile(filepath.Join(oldLeaf, receipt), []byte(strconv.FormatInt(latest, 10)+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	next := (&participant{w: w, extra: []string{"CLAUDE_CODE_SESSION_ID=" + cleared.key}}).mustPlain("inbox")
	if strings.Contains(next, "Before voluntarily ending any turn") || strings.Contains(next, "@latepeer") {
		t.Fatalf("the cleared session's next command repeated what the old one showed:\n%s", next)
	}
	var acked bool
	var seen int64
	if err := w.db().QueryRow(`SELECT hint_acked, roster_ack_seq FROM binding WHERE alias = 'claude1' AND state = 'live'`).Scan(&acked, &seen); err != nil ||
		!acked || seen < latest {
		t.Fatalf("binding hint_acked=%v roster_ack_seq=%d (%v), want true and %d", acked, seen, err, latest)
	}
}
