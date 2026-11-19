//go:build e2e

package e2e

import (
	"slices"
	"strconv"
	"strings"
	"testing"
)

// unreadOfKind returns the IDs of unread inbox items of one kind.
func unreadOfKind(t *testing.T, o outcome, kind string) []int64 {
	t.Helper()
	items, _ := o.obj["unread"].([]any)
	var ids []int64
	for _, item := range items {
		row, _ := item.(map[string]any)
		if row["kind"] == kind {
			ids = append(ids, int64(row["id"].(float64)))
		}
	}
	return ids
}

// The turn-end rule at the protocol level: a member with a shell and no
// adapter learns the rule from join and follows it with crosshall inbox. A
// request that arrived during its turn is listed as an unread request before
// it stops; a note, a reply to its own request and a roster change that
// arrived at the same time are not requests; reading the request clears it.
func TestTurnEndRuleShellOnlyMember(t *testing.T) {
	w := newWorld(t)
	owner := w.owner()
	owner.must("owner", "start")
	owner.must("owner", "channel", "create", "checkout")
	form, tax := w.member(), w.member()
	form.must("join", "checkout", "--as", "form")
	joined := tax.mustPlain("join", "checkout", "--as", "tax")
	const rule = "Before you end a turn by your own choice, run `crosshall inbox` and read each request addressed to you that it lists. What arrives after that check belongs to your next turn."
	if !strings.Contains(joined, rule) {
		t.Fatalf("join does not state the turn-end rule:\n%s", joined)
	}
	grantRoots(t, w, "tax", 1)

	// tax's turn: it asks form something of its own.
	own := num(t, tax.must("ask", "@form", "Which rounding mode?"), "id")

	// During that turn: a reply to tax's own request, a note, a roster
	// change and one request addressed to tax.
	form.must("reply", strconv.FormatInt(own, 10), "Half-even.")
	form.must("note", "@tax", "FYI: the copy changed.")
	w.member().must("join", "checkout", "--as", "lint")
	request := num(t, form.must("ask", "@tax", "Please check the totals."), "id")

	// Before it stops, tax follows the rule.
	before := tax.must("inbox")
	if got := unreadOfKind(t, before, "ask"); !slices.Equal(got, []int64{request}) {
		t.Fatalf("unread requests before the turn ends: %v, want [%d]: %s", got, request, before.stdout)
	}
	if len(unreadOfKind(t, before, "note")) != 1 || len(unreadOfKind(t, before, "reply")) != 1 {
		t.Fatalf("the note and the reply should be unread but not requests: %s", before.stdout)
	}
	plain := tax.mustPlain("inbox")
	if !strings.Contains(plain, "#"+strconv.FormatInt(request, 10)+" ask from @form") {
		t.Fatalf("plain inbox does not show the request as an ask:\n%s", plain)
	}

	// Reading the request satisfies the rule: no unread request is left, and
	// the request stays owed until tax answers it.
	tax.must("read", strconv.FormatInt(request, 10))
	after := tax.must("inbox")
	if got := unreadOfKind(t, after, "ask"); len(got) != 0 {
		t.Fatalf("a read request is still unread: %v", got)
	}
	owed, _ := after.obj["owed"].([]any)
	if len(owed) != 1 || int64(owed[0].(map[string]any)["id"].(float64)) != request {
		t.Fatalf("the read request should stay owed: %s", after.stdout)
	}
}
