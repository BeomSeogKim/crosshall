//go:build e2e

package e2e

import (
	"os/exec"
	"strings"
	"testing"
)

// The new-member command comes first. The recovery path still explains why
// a replacement session must restore its old binding before joining.
func TestATakenAliasNextStepPutsNewMemberFirst(t *testing.T) {
	w := newWorld(t)
	owner := w.owner()
	owner.must("owner", "start")
	owner.must("owner", "channel", "create", "checkout")
	w.member().must("join", "checkout", "--as", "form")
	fresh := w.member()
	cmd := exec.Command(binary, "join", "checkout", "--as", "form")
	cmd.Dir, cmd.Env = w.home, fresh.env()
	out, err := cmd.CombinedOutput()
	exit, _ := err.(*exec.ExitError)
	if exit == nil || exit.ExitCode() != 3 {
		t.Fatalf("exit %v:\n%s", err, out)
	}
	if !strings.HasPrefix(string(out), "crosshall: @form is taken in #checkout\nnext: For a new member,") {
		t.Fatalf("the new-member option did not lead the refusal:\n%s", out)
	}
	remaining := string(out)
	for _, step := range []string{
		"next: For a new member, crosshall join checkout chooses an unused alias.",
		"To recover the old membership, run crosshall whoami --full",
		"only to the owner",
		"crosshall owner who --all",
		"crosshall owner restore with that ID as its first argument and --new-key set to the token",
		"Joining under another alias makes this session's key ineligible for owner restore.",
	} {
		_, rest, found := strings.Cut(remaining, step)
		if !found {
			t.Fatalf("missing or out-of-order recovery step %q:\n%s", step, out)
		}
		remaining = rest
	}
	if o := fresh.run("inbox"); o.exit != 4 || o.obj["code"] != "not_enrolled" {
		t.Fatalf("the refused join enrolled the key: %d %v", o.exit, o.obj)
	}
}
