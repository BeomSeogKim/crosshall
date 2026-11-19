//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// A member that lost its context can see the hint again: a repeated join,
// after a later command has acknowledged the first one, prints it under
// "Already in" with the rules, and lists only roster changes.
func TestRepeatedJoinShowsTheHintAgain(t *testing.T) {
	w := newWorld(t)
	owner := w.owner()
	owner.must("owner", "start")
	owner.must("owner", "channel", "create", "checkout")
	form := w.member()
	form.must("join", "checkout", "--as", "form")
	form.must("inbox")
	again := form.mustPlain("join", "checkout")
	for _, want := range []string{
		"[crosshall] Already in #checkout as @form.",
		"Before voluntarily ending any turn while joined, run `crosshall inbox`",
		"--more-time",
	} {
		if !strings.Contains(again, want) {
			t.Errorf("repeated join lacks %q:\n%s", want, again)
		}
	}
	if strings.Contains(again, "Joined #checkout") {
		t.Errorf("repeated join claims a new join:\n%s", again)
	}
}
