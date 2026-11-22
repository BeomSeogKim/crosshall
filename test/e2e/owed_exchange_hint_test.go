//go:build e2e

package e2e

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

var owedReadRetryHint = regexp.MustCompile(`(?m)^crosshall: outcome_unknown\nnext: retry with --key ([0-9a-f]{32})\.`)

func readOwedExchangeHint(t *testing.T, member *participant, hint string) string {
	t.Helper()
	args := strings.Fields(strings.TrimPrefix(hint, "crosshall "))
	run := func(args ...string) ([]byte, error) {
		cmd := exec.Command(binary, args...)
		cmd.Dir, cmd.Env = member.w.home, member.env()
		return cmd.CombinedOutput()
	}
	out, err := run(args...)
	if exit, ok := err.(*exec.ExitError); ok && exit.ExitCode() == int(core.ExitUnavailable) {
		if match := owedReadRetryHint.FindStringSubmatch(string(out)); match != nil {
			t.Logf("crosshall %v had an unknown outcome: %s", args, out)
			out, err = run(append([]string{"--key", match[1]}, args...)...)
		}
	}
	if err != nil {
		t.Fatalf("crosshall %v: %v\n%s", args, err, out)
	}
	return string(out)
}

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
	if read := readOwedExchangeHint(t, tax, hint); !strings.Contains(read, "Check the totals.") {
		t.Fatalf("suggested command did not read the owed ask: %s", read)
	}

	second := num(t, form.must("ask", "@tax", "Check the receipts.", "--new"), "id")
	ambiguous := tax.run("ask", "@form", "Which item?")
	hint = fmt.Sprintf("crosshall read %d %d", first, second)
	if ambiguous.exit != 3 || ambiguous.obj["code"] != "ambiguous_root" || ambiguous.obj["hint"] != hint {
		t.Fatalf("unexpected ambiguous exchange refusal: %s", ambiguous.stdout)
	}
	if read := readOwedExchangeHint(t, tax, hint); !strings.Contains(read, "Check the totals.") || !strings.Contains(read, "Check the receipts.") {
		t.Fatalf("suggested command did not read both owed asks: %s", read)
	}
}
