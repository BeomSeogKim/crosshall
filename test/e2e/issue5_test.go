//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func field(t *testing.T, m map[string]any, path ...string) any {
	t.Helper()
	var cur any = m
	for _, p := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("no %v in %v", path, m)
		}
		cur = obj[p]
	}
	return cur
}

func ids(v any) []int64 {
	var out []int64
	list, _ := v.([]any)
	for _, x := range list {
		switch x := x.(type) {
		case float64:
			out = append(out, int64(x))
		case map[string]any:
			if id, ok := x["id"].(float64); ok {
				out = append(out, int64(id))
			}
		}
	}
	return out
}

func has(list []int64, id int64) bool {
	for _, x := range list {
		if x == id {
			return true
		}
	}
	return false
}

// ledgerRecipients returns the owner ledger rows of one request by alias.
func ledgerRecipients(t *testing.T, status outcome, id int64) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, ch := range status.obj["channels"].([]any) {
		for _, r := range ch.(map[string]any)["requests"].([]any) {
			req := r.(map[string]any)
			if int64(req["id"].(float64)) != id {
				continue
			}
			for _, rc := range req["recipients"].([]any) {
				row := rc.(map[string]any)
				out[row["alias"].(string)] = row
			}
		}
	}
	return out
}

func stages(row map[string]any) []string {
	var out []string
	for _, r := range row["receipts"].([]any) {
		out = append(out, r.(map[string]any)["stage"].(string))
	}
	return out
}

// Ask, read, reply and waiting between three members, with the owner's
// ledger showing each step.
func TestThreeMemberCycleAndLedger(t *testing.T) {
	w, owner, members := setup(t)
	form, tax, lint := members[0], members[1], members[2]
	grantRoots(t, w, "form", 1)
	q := form.must("ask", "@tax", "@lint", "check the refund rounding", "--due", "30m")
	id := num(t, q, "id")
	idArg := strconv.FormatInt(id, 10)

	// tax learns about it from any command, finds it in the inbox, reads it.
	if p := ids(tax.must("who").obj["pending"]); !has(p, id) {
		t.Fatalf("pending on tax's next command: %v", p)
	}
	if u := ids(field(t, tax.must("inbox").obj, "unread")); !has(u, id) {
		t.Fatalf("inbox unread %v", u)
	}
	r := tax.must("read", idArg)
	if msgs := r.obj["messages"].([]any); len(msgs) != 1 || msgs[0].(map[string]any)["body"] != "check the refund rounding" {
		t.Fatalf("read %v", r.obj)
	}
	in := tax.must("inbox")
	if u, o := ids(in.obj["unread"]), ids(in.obj["owed"]); has(u, id) || !has(o, id) {
		t.Fatalf("after read: unread %v owed %v", u, o)
	}

	tax.must("reply", idArg, "half-even is right")
	lint.must("read", idArg)
	lint.must("reply", idArg, "--more-time", "after lunch")

	// The asker still waits on lint; tax's answer is there to read.
	wt := form.must("waiting")
	if reqs := ids(wt.obj["requests"]); !has(reqs, id) {
		t.Fatalf("waiting %v", wt.obj)
	}
	if p := ids(wt.obj["pending"]); len(p) == 0 {
		t.Fatalf("the asker's pending list misses the replies: %v", wt.obj)
	}

	status := owner.must("owner", "status", "--all")
	rows := ledgerRecipients(t, status, id)
	taxRow, lintRow := rows["tax"], rows["lint"]
	if taxRow == nil || lintRow == nil {
		t.Fatalf("ledger rows %v", rows)
	}
	if taxRow["picked_up_at_ms"] == nil || taxRow["settled_at_ms"] == nil || fmt.Sprint(stages(taxRow)) != "[stored picked_up settled]" {
		t.Fatalf("tax ledger row %v", taxRow)
	}
	if lintRow["picked_up_at_ms"] == nil || lintRow["more_time_at_ms"] == nil || lintRow["settled_at_ms"] != nil ||
		fmt.Sprint(stages(lintRow)) != "[stored picked_up]" {
		t.Fatalf("lint ledger row %v", lintRow)
	}
}

// Two members asking each other back and forth are stopped by the budget;
// the owner sees one notice, and a grant lets work continue.
func TestBackAndForthIsStoppedByTheBudget(t *testing.T) {
	w, owner, members := setup(t)
	form, tax := members[0], members[1]
	grantRoots(t, w, "form", 1)
	first := form.must("ask", "@tax", "round 1")
	ex := num(t, first, "exchange")
	pair := []*participant{tax, form}
	for i := 2; i <= 8; i++ {
		o := pair[i%2].must("ask", "@"+map[*participant]string{tax: "form", form: "tax"}[pair[i%2]], fmt.Sprintf("round %d", i))
		if num(t, o, "exchange") != ex {
			t.Fatalf("round %d left the exchange: %v", i, o.obj)
		}
	}
	for i := 0; i < 3; i++ {
		o := tax.run("ask", "@form", "one more")
		if o.exit != 3 || o.obj["code"] != "budget_exhausted" {
			t.Fatalf("refusal %d: exit %d %v", i, o.exit, o.obj)
		}
	}
	// Answers stay free at zero.
	tax.must("reply", strconv.FormatInt(num(t, first, "id"), 10), "answer at zero")

	status := owner.must("owner", "status", "--all")
	notices := field(t, status.obj["channels"].([]any)[0].(map[string]any), "budget_exhaustions").([]any)
	if len(notices) != 1 {
		t.Fatalf("%d exhaustion notices for one episode: %v", len(notices), notices)
	}
	owner.must("owner", "budget", "grant", fmt.Sprintf("checkout/x%d", ex), "2")
	if o := tax.must("ask", "@form", "after the grant"); num(t, o, "exchange") != ex {
		t.Fatalf("after the grant %v", o.obj)
	}
}

// Responses lost after commit, for reply, read and an owner grant: the same
// command with the same key returns the first result and applies once.
func TestLostResponsesForReplyReadAndGrant(t *testing.T) {
	w, owner, members := setup(t)
	form, tax, lint := members[0], members[1], members[2]
	db := w.db()
	grantRoots(t, w, "form", 1)
	id := num(t, form.must("ask", "@tax", "@lint", "q"), "id")
	idArg := strconv.FormatInt(id, 10)

	lost := func(p *participant, verb, args string) (string, map[string]any) {
		t.Helper()
		gen, err := os.ReadFile(filepath.Join(p.leaf(), "gen"))
		if err != nil {
			t.Fatal(err)
		}
		key := randomHex(16)
		req := memberRequest(key, string(gen), p.keyDigest(), args)
		req.Verb = verb
		resp, data := plant(t, p.leaf(), req, true)
		if resp.Status != 0 {
			t.Fatalf("planted %s failed: %d %s", verb, resp.Status, data)
		}
		var first map[string]any
		if err := json.Unmarshal([]byte(data), &first); err != nil {
			t.Fatal(err)
		}
		return key, first
	}
	count := func(q string) int {
		t.Helper()
		var n int
		if err := db.QueryRow(q).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	key, first := lost(tax, "reply", fmt.Sprintf(`{"to":%d,"body":"fine"}`, id))
	if again := tax.must("reply", idArg, "fine", "--key", key); again.obj["id"] != first["id"] {
		t.Fatalf("reply replay %v, first %v", again.obj, first)
	}
	if n := count(`SELECT COUNT(*) FROM receipt WHERE stage = 'settled'`); n != 1 {
		t.Fatalf("%d settled receipts", n)
	}

	key, _ = lost(lint, "read", fmt.Sprintf(`{"ids":[%d]}`, id))
	if again := lint.must("read", idArg, "--key", key); len(again.obj["messages"].([]any)) != 1 {
		t.Fatalf("read replay %v", again.obj)
	}
	if n := count(`SELECT COUNT(*) FROM receipt WHERE stage = 'picked_up'`); n != 1 {
		t.Fatalf("%d pickup receipts", n)
	}

	ex := count(`SELECT exchange_id FROM message WHERE id = ` + idArg)
	gkey := randomHex(16)
	resp, data := plant(t, filepath.Join(w.state, "owner"),
		ownerRequestVerb(gkey, "owner budget grant", fmt.Sprintf(`{"channel":"checkout","exchange_id":%d,"units":3}`, ex)), true)
	if resp.Status != 0 {
		t.Fatalf("planted grant failed: %d %s", resp.Status, data)
	}
	owner.must("owner", "budget", "grant", fmt.Sprintf("checkout/x%d", ex), "3", "--key", gkey)
	if o := owner.run("owner", "budget", "grant", fmt.Sprintf("checkout/x%d", ex), "5", "--key", gkey); o.exit != 3 || o.obj["code"] != "key_conflict" {
		t.Fatalf("changed amount: exit %d %v", o.exit, o.obj)
	}
	if n := count(fmt.Sprintf(`SELECT granted FROM exchange WHERE id = %d`, ex)); n != 8+3 {
		t.Fatalf("granted %d, want 11", n)
	}
}

func ownerRequestVerb(key, verb, args string) (r wire.Request) {
	r = ownerRequest(key, args)
	r.Verb = verb
	return r
}

// Text that starts with an alias is stored as written and reaches only the
// named recipient.
func TestAliasInsideTheTextReachesOnlyTheRecipient(t *testing.T) {
	w, _, members := setup(t)
	form := members[0]
	grantRoots(t, w, "form", 1)
	id := num(t, form.must("ask", "@tax", "@lint, is this ready?"), "id")
	var body string
	var recipients int
	db := w.db()
	if err := db.QueryRow(`SELECT body FROM message WHERE id = ?`, id).Scan(&body); err != nil || body != "@lint, is this ready?" {
		t.Fatalf("stored body %q (%v)", body, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM recipient WHERE message_id = ?`, id).Scan(&recipients); err != nil || recipients != 1 {
		t.Fatalf("%d recipients (%v)", recipients, err)
	}
}
