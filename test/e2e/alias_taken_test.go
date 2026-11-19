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
	if exit == nil || exit.ExitCode() != 3 ||
		!strings.Contains(string(out), "next: if this session lost its identity, ask the owner to run crosshall owner restore first; a new session can run crosshall join checkout --as <another alias>") {
		t.Fatalf("exit %v:\n%s", err, out)
	}
	if o := fresh.run("inbox"); o.exit != 4 || o.obj["code"] != "not_enrolled" {
		t.Fatalf("the refused join enrolled the key: %d %v", o.exit, o.obj)
	}
}
