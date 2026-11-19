//go:build e2e

package e2e

import (
	"fmt"
	"strconv"
	"testing"
)

func TestLinkedHandoffUsesOneRootAndCompletesTheOriginalRequest(t *testing.T) {
	w, owner, members := setup(t)
	lead, worker, reviewer := members[0], members[1], members[2]
	grantRoots(t, w, "form", 1)
	grantRoots(t, w, "tax", 1)

	parent := lead.must("ask", "@tax", "@lint", "Check the totals.")
	parentID := num(t, parent, "id")
	parentArg := strconv.FormatInt(parentID, 10)
	parentExchange := num(t, parent, "exchange")
	worker.must("read", parentArg)
	for unit := 1; unit < 8; unit++ {
		// Fill the parent exchange without adding unread work for the worker.
		lead.mustFixtureWrite("ask", "@lint", fmt.Sprintf("Another item %d", unit), "--re", parentArg)
	}
	requireBudgetRefusal(t, worker.run("ask", "@lint", "Please review the totals.", "--new"), "must_answer_first")

	handoff := []string{"--key", "review-totals", "ask", "@lint", "Please review the totals.", "--new", "--re", parentArg}
	child := worker.must(handoff...)
	childID := num(t, child, "id")
	childArg := strconv.FormatInt(childID, 10)
	childExchange := num(t, child, "exchange")
	if child.obj["new_exchange"] != true || childExchange == parentExchange ||
		num(t, child, "parent_request_id") != parentID || num(t, child, "debit") != 1 || num(t, child, "remaining") != 7 {
		t.Fatalf("unexpected linked handoff: %s", child.stdout)
	}
	state := worker.must("status")
	if self, _ := state.obj["self"].(map[string]any); self["root_tokens"] != float64(0) {
		t.Fatalf("linked handoff did not spend the worker's single root: %s", state.stdout)
	}
	parentRecipients := ledgerRecipients(t, owner.must("owner", "status", "--channel", "checkout"), parentID)
	if parentRecipients["tax"]["state"] != "open" || parentRecipients["tax"]["picked_up_at_ms"] == nil ||
		parentRecipients["lint"]["state"] != "open" || parentRecipients["lint"]["picked_up_at_ms"] != nil {
		t.Fatalf("handoff changed the parent obligations: %+v", parentRecipients)
	}

	// Ordinary attribution stays with the parent's empty exchange. Naming
	// the handoff request explicitly lets its opener use the child exchange.
	requireBudgetRefusal(t, worker.run("ask", "@lint", "Which figures?"), "budget_exhausted")
	followUp := worker.must("ask", "@lint", "Which figures?", "--re", childArg)
	followUpArg := strconv.FormatInt(num(t, followUp, "id"), 10)
	if followUp.obj["new_exchange"] != false || num(t, followUp, "exchange") != childExchange ||
		num(t, followUp, "debit") != 1 || num(t, followUp, "remaining") != 6 {
		t.Fatalf("explicit follow-up did not use the child exchange: %s", followUp.stdout)
	}

	reviewer.must("read", childArg, followUpArg)
	reviewer.must("reply", followUpArg, "Use the final figures.")
	review := reviewer.must("reply", childArg, "The totals match.")
	worker.must("read", strconv.FormatInt(num(t, review, "id"), 10))
	worker.must("reply", parentArg, "The reviewer confirmed the totals.")

	// An operation-key replay still returns the original handoff after the
	// parent has settled, without consuming a token or another child unit.
	before := budgetRows(t, w)
	replay := worker.must(handoff...)
	if num(t, replay, "id") != childID || num(t, replay, "exchange") != childExchange ||
		num(t, replay, "parent_request_id") != parentID || num(t, replay, "remaining") != 7 {
		t.Fatalf("replay changed the linked handoff receipt: %s", replay.stdout)
	}
	if after := budgetRows(t, w); after != before {
		t.Fatalf("replay changed obligations, exchanges, budget events or tokens: %v -> %v", before, after)
	}
	ledger := checkoutLedger(t, owner)
	if len(ledger.Exchanges) != 2 || ledger.Exchanges[0].ID != parentExchange || ledger.Exchanges[0].Remaining != 0 ||
		ledger.Exchanges[1].ID != childExchange || ledger.Exchanges[1].Remaining != 6 {
		t.Fatalf("unexpected final exchange balances: %+v", ledger.Exchanges)
	}
	parentRecipients = ledgerRecipients(t, owner.must("owner", "status", "--channel", "checkout"), parentID)
	if parentRecipients["tax"]["state"] != "replied" || parentRecipients["lint"]["state"] != "open" {
		t.Fatalf("reply settled the wrong parent obligation: %+v", parentRecipients)
	}
}
