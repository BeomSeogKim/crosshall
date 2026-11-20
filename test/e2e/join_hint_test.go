//go:build e2e

package e2e

import (
	"strings"
	"testing"
)

// A repeated join, after a later command has acknowledged the first one,
// says "Already in" and names the command that prints the rules. A member
// that lost its context runs that command and sees the rules again.
func TestRepeatedJoinShowsTheHintAgain(t *testing.T) {
	w := newWorld(t)
	owner := w.owner()
	owner.must("owner", "start")
	owner.must("owner", "channel", "create", "checkout")
	form := w.member()
	form.must("join", "checkout", "--as", "form")
	form.must("inbox")
	repeated := form.mustPlain("join", "checkout")
	if !strings.Contains(repeated, "[crosshall] Already in #checkout as @form.") ||
		!strings.Contains(repeated, "`crosshall join checkout --rules` prints them again.") ||
		strings.Contains(repeated, "run `crosshall inbox`") || len(repeated) > 200 {
		t.Errorf("repeated join:\n%s", repeated)
	}
	again := form.mustPlain("join", "checkout", "--rules")
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
