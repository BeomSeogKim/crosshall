package domain

import (
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

// A request costs one unit whatever the number of recipients, so a fan-out
// spends no more of the exchange than a single question.
func TestARequestToThreeMembersCostsOneUnit(t *testing.T) {
	f, a, _, _ := channel3(t)
	wantOK(t, f.join(f.newSession("codex"), Join{Channel: "checkout", Alias: "lint"}, ""))
	f.roots(a, 1)
	got := decode[AskResult](t, f.ask(a, Ask{To: []string{"tax", "a11y", "lint"}, Body: "review"}, ""))
	if got.Debit != 1 || got.Remaining != DefaultCredit-1 || len(got.Recipients) != 3 {
		t.Fatalf("result %+v", got)
	}
	for q, want := range map[string]int{
		`SELECT spent FROM exchange`: 1,
		`SELECT COUNT(*) FROM budget_event WHERE reason = 'debit' AND delta = -1`: 1,
		`SELECT COUNT(*) FROM obligation WHERE state = 'open'`:                    3,
	} {
		if n := f.count(q); n != want {
			t.Errorf("%s = %d, want %d", q, n, want)
		}
	}
}

func TestARequestToTheWholeChannelCostsOneUnit(t *testing.T) {
	f, a, _, _ := channel3(t)
	f.roots(a, 1)
	got := decode[AskResult](t, f.ask(a, Ask{All: true, Each: true, Body: "status?"}, ""))
	if got.Debit != 1 || got.Remaining != DefaultCredit-1 || len(got.Recipients) != 2 {
		t.Fatalf("result %+v", got)
	}
}

// The budget's purpose holds: two agents asking each other in turn stop after
// the exchange's eight units, and one exhaustion event records the refusal.
func TestTwoAgentLoopStopsAfterEightRequests(t *testing.T) {
	f, a, b, _ := channel3(t)
	f.roots(a, 1)
	f.roots(b, 1)
	first := decode[AskResult](t, f.ask(a, Ask{To: []string{"tax"}, Body: "1"}, ""))
	asker, other := b, "form"
	for i := 2; i <= DefaultCredit; i++ {
		got := decode[AskResult](t, f.ask(asker, Ask{To: []string{other}, Body: itoa(int64(i))}, ""))
		if got.Exchange != first.Exchange || got.NewExchange || got.Debit != 1 || got.Remaining != int64(DefaultCredit-i) {
			t.Fatalf("request %d: %+v", i, got)
		}
		if asker == b {
			asker, other = a, "tax"
		} else {
			asker, other = b, "form"
		}
	}
	for range 3 {
		wantCode(t, f.ask(asker, Ask{To: []string{other}, Body: "one more"}, ""), core.ExitRefused, core.CodeBudgetExhausted)
	}
	if n := f.count(`SELECT COUNT(*) FROM message WHERE kind = 'ask'`); n != DefaultCredit {
		t.Fatalf("%d requests stored, want %d", n, DefaultCredit)
	}
	if n := f.count(`SELECT COUNT(*) FROM event WHERE kind = 'budget_exhausted'`); n != 1 {
		t.Fatalf("%d exhaustion events", n)
	}
}

// Without a root token the refusal gives a complete owner grant command and
// explains that replies and notes are free.
func TestBudgetRefusalNamesTheWaysOn(t *testing.T) {
	f, a, _, _ := channel3(t)
	f.roots(a, 1)
	for range DefaultCredit {
		wantOK(t, f.ask(a, Ask{To: []string{"tax"}, Body: "q"}, ""))
	}
	r := f.ask(a, Ask{To: []string{"tax", "a11y"}, Body: "over"}, "")
	wantCode(t, r, core.ExitRefused, core.CodeBudgetExhausted)
	refusal := decode[Refusal](t, r)
	if refusal.Message != "x1 has no units left; a request costs 1" {
		t.Fatalf("message %q", refusal.Message)
	}
	for _, want := range []string{"You have no root tokens.", "crosshall owner budget grant checkout/x1 1", "crosshall reply", "crosshall note"} {
		if !strings.Contains(refusal.Hint, want) {
			t.Errorf("hint %q lacks %q", refusal.Hint, want)
		}
	}
}
