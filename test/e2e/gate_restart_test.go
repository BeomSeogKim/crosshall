//go:build e2e

package e2e

import (
	"reflect"
	"slices"
	"strconv"
	"testing"
)

func TestOverdueRequestAndSpentBudgetSurviveRestart(t *testing.T) {
	w, owner, members := setup(t)
	owner.must("owner", "budget", "grant", "checkout/@form", "--roots", "1")
	ask := members[0].must("ask", "@tax", "Review the tax calculation", "--due", "1m")
	due, ok := ask.obj["due_at_ms"].(float64)
	if !ok || due <= 0 {
		t.Fatalf("ask returned no deadline: %s", ask.stdout)
	}
	members[1].must("read")

	// The offset is read when a dispatcher starts. Move forward once, then
	// use the same offset for the crash/restart comparison below.
	futureOwner := &participant{w: w, extra: []string{"CROSSHALL_TEST_CLOCK_OFFSET=2m"}}
	w.kill()
	futureOwner.must("owner", "start")

	before := owner.must("owner", "status", "--all").obj
	channel := soleLedgerRow(t, before, "channels")
	request := soleLedgerRow(t, channel, "requests")
	recipient := soleLedgerRow(t, request, "recipients")
	exchange := soleLedgerRow(t, channel, "exchanges")
	askID, ok := ask.obj["id"].(float64)
	if !ok || askID <= 0 || channel["name"] != "checkout" || request["id"] != askID || request["due_at_ms"] != due {
		t.Fatalf("status does not identify the committed ask: %v", request)
	}
	pickedUp, ok := recipient["picked_up_at_ms"].(float64)
	if !ok || pickedUp <= 0 || recipient["alias"] != "tax" || recipient["state"] != "open" || recipient["overdue_since_ms"] != due {
		t.Fatalf("expected a picked-up, open, overdue request: %v", recipient)
	}
	exchangeID, ok := exchange["id"].(float64)
	granted, hasGrant := exchange["granted"].(float64)
	if !ok || exchangeID <= 0 || request["exchange_id"] != exchangeID || !hasGrant || granted < 1 ||
		exchange["spent"] != float64(1) || exchange["remaining"] != granted-1 {
		t.Fatalf("expected the request exchange to retain its debit: %v", exchange)
	}

	w.kill()
	futureOwner.must("owner", "start")
	after := owner.must("owner", "status", "--all").obj
	for _, status := range []map[string]any{before, after} {
		if generated, ok := status["generated_at_ms"].(float64); !ok || generated < due {
			t.Fatalf("invalid ledger generation time: %v", status["generated_at_ms"])
		}
		delete(status, "generated_at_ms")
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("ledger changed across restart:\nbefore %v\nafter  %v", before, after)
	}

	t.Run("more time and late settlement", func(t *testing.T) {
		id := strconv.FormatInt(int64(askID), 10)
		members[1].must("reply", id, "--more-time", "I need another review.")
		status := owner.must("owner", "status", "--all")
		open := ledgerRecipients(t, status, int64(askID))["tax"]
		more, ok := open["more_time_at_ms"].(float64)
		if !ok || more <= due || open["overdue_since_ms"] != due || open["state"] != "open" ||
			open["settled_at_ms"] != nil || slices.Contains(stages(open), "settled") {
			t.Fatalf("more time changed the overdue obligation: %v", open)
		}

		members[1].must("reply", id, "The calculation is correct.")
		status = owner.must("owner", "status", "--all")
		request := soleLedgerRow(t, soleLedgerRow(t, status.obj, "channels"), "requests")
		row := soleLedgerRow(t, request, "recipients")
		settled, ok := row["settled_at_ms"].(float64)
		if request["id"] != askID || request["due_at_ms"] != due ||
			!ok || settled < more || row["late_by_ms"] != settled-due || row["overdue_since_ms"] != nil ||
			row["state"] != "replied" || !slices.Equal(stages(row), []string{"stored", "picked_up", "settled"}) {
			t.Fatalf("ledger lost the deadline, late reply or its evidence: %v", request)
		}
	})
}

func soleLedgerRow(t *testing.T, parent map[string]any, field string) map[string]any {
	t.Helper()
	rows, ok := parent[field].([]any)
	if !ok || len(rows) != 1 {
		t.Fatalf("expected one row in %s: %v", field, parent[field])
	}
	row, ok := rows[0].(map[string]any)
	if !ok {
		t.Fatalf("invalid row in %s: %v", field, rows[0])
	}
	return row
}
