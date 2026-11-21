//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

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
				if err := w.db().QueryRow(`SELECT launch_ref FROM observation WHERE event_ref = 'after-join:UserPromptSubmit'`).Scan(&launch); err != nil ||
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
			joined := before.must("join", "checkout")
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
