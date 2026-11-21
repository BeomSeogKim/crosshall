//go:build e2e

package e2e

import (
	"os"
	"strconv"
	"strings"
	"testing"
)

// Issue #156: /clear in Claude Code starts a new session ID in the same
// process. The new session continues the member: its alias, its open
// requests and its owed replies.
func TestClearInTheSameProcessKeepsTheMember(t *testing.T) {
	for _, mapped := range []bool{true, false} {
		t.Run("mapped="+strconv.FormatBool(mapped), func(t *testing.T) {
			w, writer, c := m2World(t)
			c.pid = strconv.Itoa(os.Getpid()) // a running process stands for Claude Code
			alias := "claude1"
			if mapped {
				c.fire("SessionStart", map[string]any{"source": "startup"})
			} else {
				// Joined by hand outside the default's repository, then resumed:
				// the resumed start binds the process.
				c.repo = w.home
				alias = "worker"
				(&participant{w: w, extra: []string{"CLAUDE_CODE_SESSION_ID=" + c.key}}).must("join", "checkout", "--as", alias)
				c.fire("SessionStart", map[string]any{"source": "resume"})
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
		})
	}
}
