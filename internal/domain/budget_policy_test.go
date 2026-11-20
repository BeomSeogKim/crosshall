package domain

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

func memberRootStatus(t *testing.T, f *fixture, s session, tokens int64, next time.Time) {
	t.Helper()
	result := f.run(s, Status{}, "")
	wantOK(t, result)
	self := decode[StatusResult](t, result).Self
	var nextMS int64
	if !next.IsZero() {
		nextMS = next.UnixMilli()
	}
	if self == nil || self.RootTokens != tokens || self.NextRootTokenAtMS != nextMS {
		t.Fatalf("member budget = %+v, want %d tokens, next %d", self, tokens, nextMS)
	}
}

func TestRootRefillAccruesAndDropsIdleTimeAtCeiling(t *testing.T) {
	f, form, _, _ := channel3(t)
	start := f.now
	next := start.Add(20 * time.Minute)
	f.now = next.Add(-time.Millisecond)
	memberRootStatus(t, f, form, 0, next)
	f.now = next
	memberRootStatus(t, f, form, 1, f.now.Add(20*time.Minute))
	memberRootStatus(t, f, form, 1, f.now.Add(20*time.Minute))
	f.now = start.Add(12 * time.Hour)
	memberRootStatus(t, f, form, 5, time.Time{})
	f.now = f.now.Add(time.Hour)
	wantOK(t, f.ask(form, Ask{To: []string{"tax"}, Body: "new exchange", New: true}, "spend-at-ceiling"))
	next = f.now.Add(20 * time.Minute)
	memberRootStatus(t, f, form, 4, next)
	f.now = next.Add(-time.Millisecond)
	memberRootStatus(t, f, form, 4, next)
	f.now = next
	memberRootStatus(t, f, form, 5, time.Time{})
}

func TestRootRefillPoliciesAreOwnerOnlyAndDoNotCreditDisabledTime(t *testing.T) {
	f, form, tax, _ := channel3(t)
	refillOff, memberRate, memberCeiling, channelRate, channelCeiling := int64(0), int64(6), int64(2), int64(3), int64(4)
	channelOff := BudgetPolicy{Channel: "checkout", Refill: &refillOff}

	wantCode(t, f.run(form, channelOff, "member-cannot-set"), core.ExitRefused, core.CodeOwnerOnly)
	defaultPolicy := decode[BudgetPolicyResult](t, f.owner(BudgetPolicy{Channel: "checkout"}, ""))
	if defaultPolicy.Refill != 3 || defaultPolicy.Ceiling != 5 {
		t.Fatalf("default policy: %+v", defaultPolicy)
	}
	wantOK(t, f.owner(channelOff, "turn-off"))
	f.now = f.now.Add(4 * time.Hour)
	memberRootStatus(t, f, form, 0, time.Time{})
	r := f.ask(form, Ask{To: []string{"tax"}, Body: "manual only", New: true}, "manual-refusal")
	wantCode(t, r, core.ExitRefused, core.CodeNoRootCredit)
	if hint := decode[Refusal](t, r).Hint; hint != "ask the owner: crosshall owner budget grant checkout/@form --roots 1" {
		t.Fatalf("manual refusal: %q", hint)
	}

	override := BudgetPolicy{Channel: "checkout", Alias: "form", Refill: &memberRate}
	wantOK(t, f.owner(override, "member-override"))
	override = BudgetPolicy{Channel: "checkout", Alias: "form", Ceiling: &memberCeiling}
	wantOK(t, f.owner(override, "member-ceiling"))
	memberStarted := f.now
	memberRootStatus(t, f, form, 0, memberStarted.Add(10*time.Minute))
	memberRootStatus(t, f, tax, 0, time.Time{})

	f.now = memberStarted.Add(5 * time.Minute)
	channelStarted := f.now
	change := BudgetPolicy{Channel: "checkout", Refill: &channelRate, Ceiling: &channelCeiling}
	wantOK(t, f.owner(change, "turn-on"))
	f.now = memberStarted.Add(10 * time.Minute)
	memberRootStatus(t, f, form, 1, memberStarted.Add(20*time.Minute))
	memberRootStatus(t, f, tax, 0, channelStarted.Add(20*time.Minute))
	f.now = channelStarted.Add(20 * time.Minute)
	memberRootStatus(t, f, form, 2, time.Time{})
	memberRootStatus(t, f, tax, 1, channelStarted.Add(40*time.Minute))
	wantOK(t, f.owner(change, "turn-on"))
	if n := f.count(`SELECT COUNT(*) FROM event WHERE kind = 'root_budget_policy'`); n != 4 {
		t.Fatalf("policy replay recorded %d changes", n)
	}

	policy := decode[BudgetPolicyResult](t, f.owner(BudgetPolicy{Channel: "checkout", Alias: "form"}, ""))
	if policy.Refill != 6 || policy.Ceiling != 2 {
		t.Fatalf("member override changed with the channel: %+v", policy)
	}
	other := decode[BudgetPolicyResult](t, f.owner(BudgetPolicy{Channel: "checkout", Alias: "tax"}, ""))
	if other.Refill != 3 || other.Ceiling != 4 {
		t.Fatalf("member failed to inherit the channel: %+v", other)
	}
}

func TestRootRefillRetainsFractionsAndManualGrants(t *testing.T) {
	f, form, _, _ := channel3(t)
	rate, ceiling := int64(7), int64(10)
	wantOK(t, f.owner(BudgetPolicy{Channel: "checkout", Refill: &rate, Ceiling: &ceiling}, "configure"))
	start := f.now
	f.now = start.Add(514285 * time.Millisecond)
	next := start.Add(514286 * time.Millisecond)
	memberRootStatus(t, f, form, 0, next)
	f.now = next
	memberRootStatus(t, f, form, 1, start.Add(1028572*time.Millisecond))
	f.now = start.Add(time.Hour)
	memberRootStatus(t, f, form, 7, start.Add(time.Hour+514286*time.Millisecond))
	grant := f.owner(BudgetGrant{Channel: "checkout", Alias: "form", Roots: 6}, "extra-roots")
	wantOK(t, grant)
	if got := decode[BudgetGrantResult](t, grant).RootTokens; got != 13 {
		t.Fatalf("manual grant capped the balance at %d", got)
	}
	f.now = f.now.Add(2 * time.Hour)
	memberRootStatus(t, f, form, 13, time.Time{})
}

func TestRootRefillStartsAtUpgradeAndSettlesPriorPolicy(t *testing.T) {
	f, form, _, _ := channel3(t)
	start := f.now
	ledgerExec(t, f, `UPDATE principal SET created_at_ms = ?, root_refill_at_ms = ? WHERE id = ?`,
		start.Add(-time.Hour).UnixMilli(), start.UnixMilli(), f.principalOf(form))
	memberRootStatus(t, f, form, 0, start.Add(20*time.Minute))
	f.now = start.Add(30 * time.Minute)
	refill := int64(6)
	wantOK(t, f.owner(BudgetPolicy{Channel: "checkout", Refill: &refill}, "faster-refill"))
	memberRootStatus(t, f, form, 1, start.Add(40*time.Minute))
	f.now = start.Add(40 * time.Minute)
	memberRootStatus(t, f, form, 2, start.Add(50*time.Minute))
}

func TestRootRefillKeepsProgressAcrossEffectiveCeilingChanges(t *testing.T) {
	for _, changeRate := range []bool{false, true} {
		t.Run(fmt.Sprintf("rate-change=%t", changeRate), func(t *testing.T) {
			f, form, tax, _ := channel3(t)
			start := f.now
			override, nextRate, ceiling := int64(3), int64(6), int64(6)
			wantOK(t, f.owner(BudgetPolicy{Channel: "checkout", Alias: "form", Refill: &override}, "member-rate"))
			f.now = start.Add(19 * time.Minute)
			update := BudgetPolicy{Channel: "checkout", Ceiling: &ceiling}
			if changeRate {
				update.Refill = &nextRate
			}
			wantOK(t, f.owner(update, "change-ceiling"))
			f.now = start.Add(20 * time.Minute)
			memberRootStatus(t, f, form, 1, start.Add(40*time.Minute))
			if changeRate {
				memberRootStatus(t, f, tax, 0, start.Add(29*time.Minute))
			} else {
				memberRootStatus(t, f, tax, 1, start.Add(40*time.Minute))
			}
		})
	}
}

func TestRootRefillIsCreditedOnceAcrossConcurrentSpends(t *testing.T) {
	f, form, _, _ := channel3(t)
	f.now = f.now.Add(20 * time.Minute)
	ready := make(chan struct{})
	out := make(chan core.Result, 2)
	var workers sync.WaitGroup
	for i := range 2 {
		workers.Go(func() {
			<-ready
			out <- f.ask(form, Ask{To: []string{"tax"}, Body: "race", New: true}, fmt.Sprintf("race-%d", i))
		})
	}
	close(ready)
	workers.Wait()
	close(out)
	var accepted, refused int
	for result := range out {
		switch {
		case result.Status == core.ExitOK:
			accepted++
		case result.Status == core.ExitRefused && result.Code == core.CodeNoRootCredit:
			refused++
		default:
			t.Fatalf("unexpected result: %+v", result)
		}
	}
	if accepted != 1 || refused != 1 || f.tokens(form) != 0 || f.count(`SELECT COUNT(*) FROM exchange`) != 1 {
		t.Fatalf("accepted = %d, refused = %d, roots = %d", accepted, refused, f.tokens(form))
	}
	if n := f.count(`SELECT COUNT(*) FROM budget_event WHERE principal_id = ? AND reason = 'roots' AND delta = 1`, f.principalOf(form)); n != 1 {
		t.Fatalf("recorded %d automatic credits", n)
	}
}

func TestRootRefillDoesNotReopenAnExhaustedExchange(t *testing.T) {
	f, form, _, _ := channel3(t)
	f.roots(form, 1)
	first := decode[AskResult](t, f.ask(form, Ask{To: []string{"tax"}, Body: "first", New: true}, "first"))
	for i := 1; i < DefaultCredit; i++ {
		wantOK(t, f.ask(form, Ask{To: []string{"tax"}, Body: "follow-up", Re: first.ID}, fmt.Sprintf("followup-%d", i)))
	}
	f.now = f.now.Add(20 * time.Minute)
	for i := range 2 {
		r := f.ask(form, Ask{To: []string{"tax"}, Body: "exhausted", Re: first.ID}, fmt.Sprintf("exhausted-%d", i))
		wantCode(t, r, core.ExitRefused, core.CodeBudgetExhausted)
		if hint := decode[Refusal](t, r).Hint; !strings.Contains(hint, "You have root tokens.") {
			t.Fatalf("refusal did not show the repleted budget: %q", hint)
		}
	}
	if f.tokens(form) != 1 || f.count(`SELECT COUNT(*) FROM message`) != DefaultCredit ||
		f.count(`SELECT COUNT(*) FROM event WHERE kind = 'budget_exhausted'`) != 1 {
		t.Fatal("automatic credit changed the exhausted exchange or duplicated its owner event")
	}
	wantOK(t, f.ask(form, Ask{To: []string{"tax"}, Body: "fresh exchange", New: true}, "fresh"))
}
