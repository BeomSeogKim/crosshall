//go:build e2e

package e2e

import (
	"os/exec"
	"strings"
	"testing"
)

func TestAskMissingRecipientPrefix(t *testing.T) {
	w := newWorld(t)
	owner, member := w.owner(), w.member()
	owner.must("owner", "channel", "create", "demo")
	member.must("join", "demo", "--as", "writer")
	w.member().must("join", "demo", "--as", "reviewer")
	cmd := exec.Command(binary, "ask", "reviewer", "text")
	cmd.Dir, cmd.Env = w.home, member.env()
	out, err := cmd.CombinedOutput()
	exit, ok := err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 2 || !strings.Contains(string(out), "use @reviewer") || !strings.Contains(string(out), "next: crosshall who") {
		t.Fatalf("missing recipient prefix: %v, %s", err, out)
	}
	if roster := member.mustPlain("who"); !strings.Contains(roster, "@reviewer") {
		t.Fatalf("suggested command did not list the recipient: %s", roster)
	}
	o := member.run("ask", "reviewer", "text")
	if o.exit != 2 || o.obj["code"] != "usage" || o.obj["hint"] != "crosshall who" {
		t.Fatalf("unexpected JSON error: %s", o.stdout)
	}
	withRecipient := member.run("ask", "one", "two", "@reviewer")
	if withRecipient.exit != 2 || !strings.Contains(withRecipient.stdout, "give the text once") {
		t.Fatalf("recipient after the text was overlooked: %s", withRecipient.stdout)
	}
	owner.must("owner", "budget", "grant", "demo/@writer", "--roots", "1")
	member.must("ask", "@reviewer", "text")
}

func TestPlainAskNamesWhoseExchangeItUses(t *testing.T) {
	w, _, members := setup(t)
	form, tax, lint := members[0], members[1], members[2]
	grantRoots(t, w, "form", 1)
	form.must("ask", "@tax", "Check the totals.", "--new")

	// Tax has no roots. An unqualified follow-up spends a unit of Form's exchange.
	if out := tax.mustPlain("ask", "@lint", "Please check the count."); !strings.Contains(out, "exchange x1 (opened by @form): 6 left.") || !strings.Contains(out, "Run crosshall status") {
		t.Fatalf("missing exchange opener: %s", out)
	}
	if status := tax.mustPlain("status"); !strings.Contains(status, "x1") {
		t.Fatalf("suggested command did not show the exchange: %s", status)
	}
	if out := form.mustPlain("ask", "@lint", "Send the figures."); !strings.Contains(out, "exchange x1: 5 left.") {
		t.Fatalf("unexpected receipt for own exchange: %s", out)
	}
	key := randomHex(16)
	command := []string{"--key", key, "ask", "@lint", "Check the final count."}
	sent := tax.must(command...)
	if sent.obj["exchange_opener"] != "form" || sent.obj["exchange_opener_departed"] != nil {
		t.Fatalf("unexpected opener at send: %s", sent.stdout)
	}
	form.must("leave")
	replacement := w.member()
	replacement.must("join", "checkout", "--as", "form")
	o := tax.must(command...)
	if o.obj["id"] != sent.obj["id"] || o.obj["remaining"] != sent.obj["remaining"] || o.obj["exchange_opener"] != "form" || o.obj["exchange_opener_departed"] != true {
		t.Fatalf("the new holder of the opener's alias must not get credit: %s", o.stdout)
	}
	if out := lint.mustPlain("ask", "@tax", "Which figures?"); !strings.Contains(out, "opened by @form, now departed") {
		t.Fatalf("ambiguous alias after departure: %s", out)
	}
}
