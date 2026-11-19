//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

func TestJoinExplainsAnUnknownCLIVersion(t *testing.T) {
	w := newWorld(t)
	w.owner().must("owner", "channel", "create", "demo")
	reviewer := &participant{w: w, extra: []string{"CLAUDE_CODE_SESSION_ID=reviewer-session"}}

	join := []string{"--key", "enroll-once", "join", "demo", "--as", "reviewer"}
	checkReceipt := func(out string) {
		t.Helper()
		for _, want := range []string{"Joined #demo as @reviewer", "Join succeeded. Crosshall could not detect the Claude Code version", "No action is needed for this warning."} {
			if !strings.Contains(out, want) {
				t.Errorf("join output lacks %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "Your membership is active") || strings.Contains(out, "Run `crosshall whoami` to check your current enrollment.") {
			t.Errorf("warning implies a current membership check is needed:\n%s", out)
		}
	}
	checkReceipt(reviewer.mustPlain(join...))
	if got := reviewer.must("whoami"); got.obj["enrollment"] != "enrolled" {
		t.Errorf("whoami did not confirm membership: %s", got.stdout)
	}

	reviewer.must("leave")
	checkReceipt(reviewer.mustPlain(join...))
	if got := reviewer.run("whoami"); got.exit != 4 || got.obj["enrollment"] != "not_enrolled" {
		t.Errorf("whoami after replay: exit %d, %s", got.exit, got.stdout)
	}
}
