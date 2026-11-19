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
		for _, want := range []string{"Joined #demo as @reviewer", "could not detect the claude CLI version", "Run `crosshall whoami` to check your current enrollment."} {
			if !strings.Contains(out, want) {
				t.Errorf("join output lacks %q:\n%s", want, out)
			}
		}
		if strings.Contains(out, "Your membership is active") {
			t.Errorf("receipt claims current membership:\n%s", out)
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
