package domain

import (
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

func TestLinkedHandoffRequiresTheCallersReadOpenRequestOnAnEmptyExchange(t *testing.T) {
	f, sender, worker, _ := channel3(t)
	f.roots(sender, 1)
	parent := decode[AskResult](t, f.ask(sender, Ask{To: []string{"tax"}, Body: "report"}, ""))
	cmd := Ask{To: []string{"a11y"}, Body: "review the report", New: true, Re: parent.ID}
	refusal := f.ask(worker, cmd, "")
	wantCode(t, refusal, core.ExitRefused, core.CodeMustAnswerFirst)
	if hint := decode[Refusal](t, refusal).Hint; hint != "crosshall read "+itoa(parent.ID) {
		t.Fatalf("unread parent hint %q", hint)
	}
	wantCode(t, f.ask(sender, cmd, ""), core.ExitRefused, core.CodeBadRoot)
	wantOK(t, actLedger(f, worker, Read{IDs: []int64{parent.ID}}, ""))
	wantCode(t, f.ask(worker, cmd, ""), core.ExitRefused, core.CodeBadRoot)

	other := decode[AskResult](t, f.ask(sender, Ask{To: []string{"tax"}, Body: "another question"}, ""))
	for range DefaultCredit - 2 {
		wantOK(t, f.ask(sender, Ask{To: []string{"a11y"}, Body: "related request"}, ""))
	}
	before := f.state()
	refusal = f.ask(worker, cmd, "")
	wantCode(t, refusal, core.ExitRefused, core.CodeMustAnswerFirst)
	if hint := decode[Refusal](t, refusal).Hint; hint != "crosshall read "+itoa(other.ID) {
		t.Fatalf("other unread request hint %q", hint)
	}
	if f.state() != before {
		t.Fatal("unread refusal wrote state")
	}
	wantOK(t, actLedger(f, worker, Read{IDs: []int64{other.ID}}, ""))
	readState := f.state()
	wantCode(t, f.ask(worker, cmd, ""), core.ExitRefused, core.CodeNoRootCredit)
	if f.state() != readState {
		t.Fatal("zero-credit refusal wrote state")
	}
	wantOK(t, f.reply(worker, Reply{To: parent.ID, Body: "done"}, ""))
	f.roots(worker, 1)
	wantCode(t, f.ask(worker, cmd, ""), core.ExitRefused, core.CodeBadRoot)
	if f.tokens(worker) != 1 {
		t.Fatal("a settled request spent a root token")
	}
}

func TestLinkedHandoffStillStopsAtTheExchangeAndRootLimits(t *testing.T) {
	f, sender, worker, _ := channel3(t)
	f.roots(sender, 1)
	f.roots(worker, 1)
	parent := decode[AskResult](t, f.ask(sender, Ask{To: []string{"tax"}, Body: "report"}, ""))
	wantOK(t, actLedger(f, worker, Read{IDs: []int64{parent.ID}}, ""))
	for range DefaultCredit - 1 {
		wantOK(t, f.ask(sender, Ask{To: []string{"a11y"}, Body: "related request"}, ""))
	}

	refusal := f.ask(worker, Ask{To: []string{"a11y"}, Body: "review"}, "")
	wantCode(t, refusal, core.ExitRefused, core.CodeBudgetExhausted)
	hint := decode[Refusal](t, refusal).Hint
	if !strings.Contains(hint, "--new --re "+itoa(parent.ID)) || !strings.Contains(hint, "crosshall owner budget grant") {
		t.Fatalf("exhaustion omits the linked handoff or owner grant: %q", hint)
	}
	cmd := Ask{To: []string{"a11y", "form"}, Body: "review", New: true, Re: parent.ID}
	before := f.state()
	caller := f.caller(worker)
	ledgerExec(t, f, `CREATE TRIGGER stop_handoff_debit BEFORE INSERT ON budget_event
		WHEN NEW.reason = 'debit' BEGIN SELECT RAISE(ABORT, 'injected debit failure'); END`)
	tx, err := f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.svc.Handle(t.Context(), tx, caller, Request{Command: cmd, OpKey: "handoff"})
	if rollbackErr := tx.Rollback(); rollbackErr != nil {
		t.Fatal(rollbackErr)
	}
	if err == nil || !strings.Contains(err.Error(), "injected debit failure") || f.state() != before || f.tokens(worker) != 1 {
		t.Fatalf("failed handoff did not roll back its root, exchange and first ask: %v", err)
	}
	ledgerExec(t, f, "DROP TRIGGER stop_handoff_debit")

	child := decode[AskResult](t, f.ask(worker, cmd, "handoff"))
	if child.Exchange == parent.Exchange || child.ParentRequestID != parent.ID || child.Remaining != DefaultCredit-1 ||
		child.Debit != 1 || len(child.Recipients) != 2 || f.tokens(worker) != 0 {
		t.Fatalf("handoff spent the wrong credit or lost its link: %+v", child)
	}
	plain := Ask{To: []string{"a11y"}, Body: "follow up"}
	wantCode(t, f.ask(worker, plain, ""), core.ExitRefused, core.CodeBudgetExhausted)
	plain.Re = child.ID
	for range DefaultCredit - 1 {
		wantOK(t, f.ask(worker, plain, ""))
	}
	wantCode(t, f.ask(worker, plain, ""), core.ExitRefused, core.CodeBudgetExhausted)
	wantCode(t, f.ask(worker, cmd, "another-handoff"), core.ExitRefused, core.CodeNoRootCredit)
	if total := f.count("SELECT SUM(spent) FROM exchange"); total != 2*DefaultCredit {
		t.Fatalf("two funded exchanges spent %d units", total)
	}
}
