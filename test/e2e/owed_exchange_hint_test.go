//go:build e2e

package e2e

import (
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

func TestOwedExchangeHintRunsAsShown(t *testing.T) {
	w, _, members := setup(t)
	form, tax := members[0], members[1]
	grantRoots(t, w, "form", 2)
	first := num(t, form.must("ask", "@tax", "Check the totals."), "id")
	followUp := []string{"ask", "@form", "Which totals?", "--new"}
	cmd := exec.Command(binary, followUp...)
	cmd.Dir, cmd.Env = w.home, tax.env()
	out, err := cmd.CombinedOutput()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 3 {
		t.Fatalf("expected --new refusal: %v, %s", err, out)
	}
	hint := fmt.Sprintf("crosshall read %d", first)
	if !strings.Contains(string(out), "\nnext: "+hint+"\n") {
		t.Fatalf("missing runnable hint %q in %s", hint, out)
	}
	refused := tax.run(followUp...)
	if refused.exit != 3 || refused.obj["code"] != "must_answer_first" || refused.obj["hint"] != hint {
		t.Fatalf("unexpected JSON refusal: %s", refused.stdout)
	}
	if read := tax.mustPlain(strings.Fields(strings.TrimPrefix(hint, "crosshall "))...); !strings.Contains(read, "Check the totals.") {
		t.Fatalf("suggested command did not read the owed ask: %s", read)
	}

	second := num(t, form.must("ask", "@tax", "Check the receipts.", "--new"), "id")
	ambiguous := tax.run("ask", "@form", "Which item?")
	hint = fmt.Sprintf("crosshall read %d %d", first, second)
	if ambiguous.exit != 3 || ambiguous.obj["code"] != "ambiguous_root" || ambiguous.obj["hint"] != hint {
		t.Fatalf("unexpected ambiguous exchange refusal: %s", ambiguous.stdout)
	}
	if read := tax.mustPlain(strings.Fields(strings.TrimPrefix(hint, "crosshall "))...); !strings.Contains(read, "Check the totals.") || !strings.Contains(read, "Check the receipts.") {
		t.Fatalf("suggested command did not read both owed asks: %s", read)
	}
}
