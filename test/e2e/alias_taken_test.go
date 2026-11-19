//go:build e2e

package e2e

import (
	"os/exec"
	"strings"
	"testing"
)

// A fresh session refused an alias is told, in its plain next step, to ask
// for a restore before taking another alias, and the refusal leaves its key
// unenrolled and so still restorable.
func TestATakenAliasNextStepPutsRestoreFirst(t *testing.T) {
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
	remaining := string(out)
	for _, step := range []string{
		"next: To recover the old membership, run crosshall whoami --full",
		"only to the owner",
		"crosshall owner who --all",
		"crosshall owner restore with that ID as its first argument and --new-key set to the token",
		"crosshall join checkout chooses an unused alias",
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
