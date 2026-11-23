//go:build e2e

package e2e

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

func TestLinkedHandoffKeepsTheParentAndCompletesTheOriginalRequest(t *testing.T) {
	_, owner, members := setup(t)
	form, worker, lint := members[0], members[1], members[2]
	parent := form.must("ask", "@tax", "Summarize the report.")
	parentID := num(t, parent, "id")
	ref := strconv.FormatInt(parentID, 10)
	command := []string{"ask", "@lint", "Review the totals.", "--new", "--re", ref, "--key", "handoff"}
	handoff := worker.must(command...)
	child := num(t, handoff, "id")
	exchange := num(t, handoff, "exchange")
	if handoff.obj["new_exchange"] != true || exchange == num(t, parent, "exchange") ||
		num(t, handoff, "parent_request_id") != parentID || num(t, handoff, "round") != 2 {
		t.Fatalf("handoff lost its root, parent or level: %s", handoff.stdout)
	}
	lint.must("reply", strconv.FormatInt(child, 10), "The totals match.")
	worker.must("reply", ref, "Report ready.")
	if replay := worker.must(command...); num(t, replay, "id") != child || num(t, replay, "round") != 2 ||
		num(t, replay, "parent_request_id") != parentID {
		t.Fatalf("handoff replay changed: %s", replay.stdout)
	}
	if row := ledgerRecipients(t, owner.must("owner", "status"), parentID)["tax"]; row["state"] != "replied" {
		t.Fatalf("parent was not settled by its recipient: %v", row)
	}
	plain := owner.mustPlain("owner", "status")
	if !strings.Contains(plain, fmt.Sprintf("x%d: round 2 of 8, handoff for #%d", exchange, parentID)) {
		t.Fatalf("handoff missing from owner view: %s", plain)
	}
}
