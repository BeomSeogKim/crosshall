//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/domain"
)

func checkoutLedger(t *testing.T, owner *participant) domain.ChannelStatus {
	t.Helper()
	out := owner.must("owner", "status", "--channel", "checkout")
	var status domain.StatusResult
	if err := json.Unmarshal([]byte(out.stdout), &status); err != nil || len(status.Channels) != 1 || status.Channels[0].Name != "checkout" {
		t.Fatalf("expected the checkout ledger: %s (%v)", out.stdout, err)
	}
	return status.Channels[0]
}

func requireBudgetRefusal(t *testing.T, out outcome, code string) {
	t.Helper()
	if out.exit != 3 || out.obj["code"] != code {
		t.Fatalf("expected refusal %s, got exit %d: %s %s", code, out.exit, out.stdout, out.stderr)
	}
}

func TestExhaustedExchangeAndRootTokens(t *testing.T) {
	w, owner, members := setup(t)
	form, tax := members[0], members[1]
	grantRoots(t, w, "form", 1)
	var asks []string
	var exchange int64
	for i := 0; i < 4; i++ {
		q := form.must("ask", "--all", "--each", fmt.Sprintf("Review issue %d", i))
		asks = append(asks, strconv.FormatInt(num(t, q, "id"), 10))
		exchange = num(t, q, "exchange")
	}
	requireBudgetRefusal(t, tax.run("ask", "--new", "@form", "Another topic"), "must_answer_first")
	for i := 0; i < 3; i++ {
		requireBudgetRefusal(t, tax.run("ask", "@form", "A follow-up"), "budget_exhausted")
	}
	ledger := checkoutLedger(t, owner)
	if len(ledger.BudgetExhaustions) != 1 || ledger.BudgetExhaustions[0].ExchangeID != exchange || ledger.BudgetExhaustions[0].Episode != 1 {
		t.Fatalf("unexpected exhaustion events: %+v", ledger.BudgetExhaustions)
	}
	plain := owner.mustPlain("owner", "status", "--channel", "checkout")
	if strings.Count(plain, " exhausted at ") != 1 || !strings.Contains(plain, fmt.Sprintf("x%d exhausted at ", exchange)) ||
		!strings.Contains(plain, "(episode 1)") {
		t.Fatalf("plain owner status: %s", plain)
	}

	tax.must("read")
	tax.must("reply", asks[0], "Reviewed")
	tax.must("reply", asks[1], "--decline")
	for _, id := range asks[2:] {
		owner.must("owner", "cancel", "checkout/"+id)
	}
	requireBudgetRefusal(t, tax.run("ask", "--new", "@form", "A new topic"), "no_root_credit")
	grantRoots(t, w, "tax", 1)
	beforeFresh := budgetRows(t, w)
	fresh := w.member()
	fresh.must("join", "checkout", "--as", "newcomer")
	requireBudgetRefusal(t, fresh.run("ask", "@form", "First question"), "no_root_credit")
	if afterFresh := budgetRows(t, w); beforeFresh != afterFresh {
		t.Fatalf("a fresh identity changed obligations, exchanges, budget events or tokens: %v -> %v", beforeFresh, afterFresh)
	}
	rechecked := checkoutLedger(t, owner)
	if len(rechecked.Exchanges) != 1 || rechecked.Exchanges[0].Remaining != 0 || rechecked.Exchanges[0].Spent != 8 || !slices.Equal(rechecked.Exchanges, ledger.Exchanges) || !slices.Equal(rechecked.BudgetExhaustions, ledger.BudgetExhaustions) {
		t.Fatalf("settlement or a fresh identity changed the exchange: %+v", rechecked.Exchanges)
	}

	// A grant ends the episode. Spending its one unit allows a new episode.
	owner.must("owner", "budget", "grant", fmt.Sprintf("checkout/x%d", exchange), "1")
	form.must("ask", "@lint", "Last question in the exchange")
	requireBudgetRefusal(t, form.run("ask", "@lint", "Extra question"), "budget_exhausted")
	beforeRestart := checkoutLedger(t, owner)
	if len(beforeRestart.BudgetExhaustions) != 2 || beforeRestart.BudgetExhaustions[1].ExchangeID != exchange || beforeRestart.BudgetExhaustions[1].Episode != 2 {
		t.Fatalf("expected a second exhaustion after the unit grant: %+v", beforeRestart.BudgetExhaustions)
	}

	w.kill()
	owner.must("owner", "start")
	afterRestart := checkoutLedger(t, owner)
	if !slices.Equal(afterRestart.Exchanges, beforeRestart.Exchanges) || !slices.Equal(afterRestart.BudgetExhaustions, beforeRestart.BudgetExhaustions) {
		t.Fatalf("restart changed the exchange or exhaustion history: %+v", afterRestart)
	}
	requireBudgetRefusal(t, fresh.run("ask", "@form", "Try again"), "no_root_credit")
	funded := tax.must("ask", "--new", "@form", "An authorized new topic")
	if funded.obj["new_exchange"] != true || num(t, funded, "exchange") == exchange || num(t, funded, "debit") != 1 || num(t, funded, "remaining") != 7 {
		t.Fatalf("unexpected new exchange: %s", funded.stdout)
	}
	requireBudgetRefusal(t, tax.run("ask", "--new", "@form", "A second topic"), "no_root_credit")
}

func TestInsufficientFanoutKeepsPartialCredit(t *testing.T) {
	w, owner, members := setup(t)
	form := members[0]
	for _, alias := range []string{"schema", "copy", "index"} {
		w.member().must("join", "checkout", "--as", alias)
	}
	grantRoots(t, w, "form", 1)
	form.must("ask", "--all", "--each", "Review the draft")
	form.must("ask", "@tax", "One more detail")
	before := checkoutLedger(t, owner)
	if len(before.Exchanges) != 1 || before.Exchanges[0].Remaining != 2 || len(before.BudgetExhaustions) != 0 {
		t.Fatalf("unexpected starting balance: %+v", before.Exchanges)
	}
	out := form.run("ask", "--all", "--each", "Review the revision")
	requireBudgetRefusal(t, out, "insufficient_credit")
	message, _ := out.obj["message"].(string)
	if !strings.Contains(message, "2 left") || !strings.Contains(message, "needs 5") {
		t.Fatalf("refusal omits remaining or required units: %s", message)
	}
	after := checkoutLedger(t, owner)
	if !reflect.DeepEqual(after.Requests, before.Requests) || !slices.Equal(after.Exchanges, before.Exchanges) || len(after.BudgetExhaustions) != 0 {
		t.Fatal("partial-credit refusal changed the requests, balance or exhaustion history")
	}
	if sent := form.must("ask", "@tax", "A smaller revision"); num(t, sent, "debit") != 1 || num(t, sent, "remaining") != 1 {
		t.Fatalf("smaller send failed to use the remaining credit: %s", sent.stdout)
	}
}

func TestConcurrentAsksSpendLastUnitOnce(t *testing.T) {
	w, owner, members := setup(t)
	form := members[0]
	grantRoots(t, w, "form", 1)
	for i := 0; i < 3; i++ {
		form.must("ask", "--all", "--each", fmt.Sprintf("Section %d", i))
	}
	last := form.must("ask", "@tax", "Clarify a section")
	if num(t, last, "remaining") != 1 {
		t.Fatalf("expected one unit before the race: %s", last.stdout)
	}

	// Both callers owe work in this exchange and have independent CLI sessions.
	start := make(chan struct{})
	var workers sync.WaitGroup
	var replies [2]outcome
	for i, p := range members[1:] {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			replies[i] = p.run("ask", "@form", "A follow-up")
		}()
	}
	close(start)
	workers.Wait()

	wins := 0
	var winner int64
	for _, r := range replies {
		if r.exit != 0 {
			requireBudgetRefusal(t, r, "budget_exhausted")
			continue
		}
		wins++
		winner = num(t, r, "id")
	}
	if wins != 1 {
		t.Fatalf("expected exactly one successful ask, got %d", wins)
	}
	ledger := checkoutLedger(t, owner)
	if len(ledger.Exchanges) != 1 || ledger.Exchanges[0].Spent != 8 || ledger.Exchanges[0].Remaining != 0 || len(ledger.Requests) != 5 {
		t.Fatalf("race did not commit exactly one debit and request: %+v", ledger)
	}
	for _, req := range ledger.Requests {
		if req.ID == winner && len(req.Recipients) == 1 && req.Recipients[0].Alias == "form" && req.Recipients[0].State == "open" {
			return
		}
	}
	t.Fatalf("owner ledger lacks the winner's single open obligation for request %d", winner)
}
