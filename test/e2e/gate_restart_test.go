//go:build e2e

package e2e

import (
	"reflect"
	"testing"
	"time"
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

	// Use the public command's minimum duration and the committed deadline.
	wait := time.Until(time.UnixMilli(int64(due)))
	if wait > time.Minute+time.Second {
		t.Fatalf("unexpected deadline for a one-minute ask: %v", wait)
	}
	time.Sleep(wait + time.Millisecond)

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
	owner.must("owner", "start")
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
