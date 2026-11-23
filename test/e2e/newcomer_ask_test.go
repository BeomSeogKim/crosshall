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
	member.must("ask", "@reviewer", "text")
}

func TestAskJSONKeepsOriginalOpenerAcrossAliasReuse(t *testing.T) {
	w, _, members := setup(t)
	form, tax, lint := members[0], members[1], members[2]
	form.must("ask", "@tax", "Check the totals.", "--new")

	if out := tax.mustPlain("ask", "@lint", "Please check the count."); !strings.Contains(out, "#2 stored. due ") || strings.Contains(out, "exchange ") {
		t.Fatalf("unexpected plain ask receipt: %s", out)
	}
	if status := tax.mustPlain("status"); !strings.Contains(status, "Round 1 of 8 on x1.") {
		t.Fatalf("member status omitted the current thread's round: %s", status)
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
	if o.obj["id"] != sent.obj["id"] || o.obj["round"] != sent.obj["round"] || o.obj["exchange_opener"] != "form" || o.obj["exchange_opener_departed"] != true {
		t.Fatalf("the new holder of the opener's alias must not get credit: %s", o.stdout)
	}
	if o := lint.must("ask", "@tax", "Which figures?"); o.obj["exchange_opener"] != "form" || o.obj["exchange_opener_departed"] != true {
		t.Fatalf("ambiguous alias after departure: %s", o.stdout)
	}
}
