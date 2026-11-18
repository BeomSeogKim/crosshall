//go:build e2e

package e2e

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// A note to the whole channel through the binary: every live member but the sender gets the
// note, nobody owes anything and no budget moves.
func TestNoteToTheWholeChannelEndToEnd(t *testing.T) {
	w, owner, members := setup(t)
	form, tax, lint := members[0], members[1], members[2]
	gone := w.member()
	gone.must("join", "checkout", "--as", "gone")
	gone.must("leave")
	before := budgetRows(t, w)

	n := form.must("note", "--all", "deploy freeze at 17:00")
	id := num(t, n, "id")
	var got []string
	for _, r := range n.obj["recipients"].([]any) {
		got = append(got, fmt.Sprint(r.(map[string]any)["alias"]))
	}
	if !slices.Equal(got, []string{"lint", "tax"}) {
		t.Fatalf("audience %v", got)
	}
	if after := budgetRows(t, w); after != before {
		t.Fatalf("a note touched obligations or budget: %v -> %v", before, after)
	}

	tax.must("read", strconv.FormatInt(id, 10))
	if u := ids(field(t, lint.must("inbox").obj, "unread")); !has(u, id) {
		t.Fatalf("lint's copy is unread on its own: %v", u)
	}
	if reqs := ids(form.must("waiting").obj["requests"]); len(reqs) != 0 {
		t.Fatalf("a note is waited on: %v", reqs)
	}
	rows := ledgerRecipients(t, owner.must("owner", "status", "--all"), id)
	if len(rows) != 0 {
		t.Fatalf("a note shows in the request ledger: %v", rows)
	}
}

// budgetRows counts what a note must leave alone.
func budgetRows(t *testing.T, w *world) [4]int {
	t.Helper()
	var out [4]int
	for i, q := range []string{
		`SELECT COUNT(*) FROM obligation`, `SELECT COUNT(*) FROM exchange`,
		`SELECT COUNT(*) FROM budget_event`, `SELECT COALESCE(SUM(root_tokens), 0) FROM principal`,
	} {
		if err := w.db().QueryRow(q).Scan(&out[i]); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// Channel isolation through the binary.
func TestChannelIsolationEndToEnd(t *testing.T) {
	w, owner, members := setup(t)
	form := members[0]
	owner.must("owner", "channel", "create", "inventory")
	stock, twin := w.member(), w.member()
	stock.must("join", "inventory", "--as", "stock")
	twin.must("join", "inventory", "--as", "tax") // the same alias in another channel
	secret := num(t, stock.must("note", "@tax", "inventory only"), "id")
	form.must("note", "@tax", "checkout only")

	if a := sortedAliases(form.must("who")); slices.Contains(a, "stock") {
		t.Fatalf("who leaks %v", a)
	}
	log := form.must("log", "--since", "0")
	for _, m := range log.obj["messages"].([]any) {
		e := m.(map[string]any)
		if int64(e["id"].(float64)) == secret || strings.Contains(fmt.Sprint(e["body"]), "inventory") {
			t.Fatalf("log leaks %v", e)
		}
	}
	if o := form.run("note", "@stock", "hi"); o.exit != 3 || o.obj["code"] != "unknown_alias" {
		t.Fatalf("note across channels: exit %d %v", o.exit, o.obj)
	}
	if o := form.run("read", strconv.FormatInt(secret, 10)); o.exit != 3 || o.obj["code"] != "not_found" {
		t.Fatalf("read across channels: exit %d %v", o.exit, o.obj)
	}
	if u := ids(field(t, members[1].must("inbox").obj, "unread")); has(u, secret) {
		t.Fatalf("@tax in checkout got inventory's note: %v", u)
	}
}

// Leave, re-join and the one-channel rule through the binary, with log, focus and leave on the way.
func TestLeaveRejoinAndOneChannelEndToEnd(t *testing.T) {
	w, owner, members := setup(t)
	form, tax := members[0], members[1]
	owner.must("owner", "channel", "create", "inventory")
	grantRoots(t, w, "form", 1)
	ask := num(t, form.must("ask", "@tax", "review?"), "id")

	tax.must("focus", "refund rounding")
	for _, m := range form.must("who").obj["members"].([]any) {
		if row := m.(map[string]any); row["alias"] == "tax" && row["focus"] != "refund rounding" {
			t.Fatalf("focus not shown: %v", row)
		}
	}
	left := tax.must("leave")
	if left.obj["owed"] != float64(1) {
		t.Fatalf("leave %v", left.obj)
	}
	if o := tax.run("inbox"); o.exit != 4 {
		t.Fatalf("a departed member's inbox: exit %d %v", o.exit, o.obj)
	}
	rows := ledgerRecipients(t, owner.must("owner", "status", "--all"), ask)
	if rows["tax"] == nil || rows["tax"]["state"] != "open" {
		t.Fatalf("the owed request must stay visible: %v", rows)
	}
	if o := tax.run("join", "inventory"); o.exit != 3 || o.obj["code"] != "one_channel" {
		t.Fatalf("join another channel: exit %d %v", o.exit, o.obj)
	}
	back := tax.must("join", "checkout")
	if back.obj["alias"] != "tax" {
		t.Fatalf("re-join %v", back.obj)
	}
	tax.must("reply", strconv.FormatInt(ask, 10), "done")
	entries := form.must("log").obj["messages"].([]any)
	last := entries[len(entries)-1].(map[string]any)
	if last["kind"] != "reply" || last["body"] != "done" {
		t.Fatalf("log tail %v", last)
	}
}
