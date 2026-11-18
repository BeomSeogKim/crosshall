//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/domain"
)

// grantRoots gives a member root tokens through the owner command.
func grantRoots(t *testing.T, w *world, alias string, n int) {
	t.Helper()
	w.owner().must("owner", "budget", "grant", "checkout/@"+alias, "--roots", strconv.Itoa(n))
}

func num(t *testing.T, o outcome, field string) int64 {
	t.Helper()
	v, ok := o.obj[field].(float64)
	if !ok {
		t.Fatalf("no %s in %v", field, o.obj)
	}
	return int64(v)
}

func TestAskAndReplyEndToEnd(t *testing.T) {
	w, _, members := setup(t)
	form, tax, lint := members[0], members[1], members[2]
	db := w.db()

	if o := form.run("ask", "@tax", "review?"); o.exit != 3 || o.obj["code"] != "no_root_credit" {
		t.Fatalf("ask without a root token: exit %d %v", o.exit, o.obj)
	}
	grantRoots(t, w, "form", 1)

	q := form.must("ask", "@tax", "@lint", "review refund rounding", "--due", "10m")
	id := num(t, q, "id")
	if q.obj["debit"] != float64(2) || q.obj["remaining"] != float64(6) || q.obj["new_exchange"] != true {
		t.Fatalf("ask result %v", q.obj)
	}

	// tax answers, lint asks for more time; form's follow-up stays in the exchange.
	a := tax.must("reply", strconv.FormatInt(id, 10), "rounding is half-even, fine")
	if a.obj["obligation"] != "replied" || a.obj["to"] != "form" {
		t.Fatalf("reply %v", a.obj)
	}
	if o := tax.run("reply", strconv.FormatInt(id, 10), "again"); o.exit != 3 || o.obj["code"] != "already_settled" {
		t.Fatalf("second reply: exit %d %v", o.exit, o.obj)
	}
	mt := lint.must("reply", "#"+strconv.FormatInt(id, 10), "--more-time", "after lunch")
	if mt.obj["obligation"] != "open" {
		t.Fatalf("more time %v", mt.obj)
	}
	f := form.must("ask", "@tax", "thanks, one more question")
	if num(t, f, "exchange") != num(t, q, "exchange") || f.obj["new_exchange"] != false {
		t.Fatalf("follow-up left the exchange: %v", f.obj)
	}

	var settled, open int
	if err := db.QueryRow(`SELECT COUNT(*) FROM receipt WHERE stage = 'settled'`).Scan(&settled); err != nil || settled != 1 {
		t.Fatalf("%d settled receipts (%v), want 1: more time must not settle", settled, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM obligation WHERE state = 'open'`).Scan(&open); err != nil || open != 2 {
		t.Fatalf("%d open obligations (%v), want 2", open, err)
	}
}

// The dispatcher commits an ask but its reply file disappears; running the
// same command after a pickup preserves the original send and shows the
// current delivery state for its frozen recipients.
func TestLostAskResponseDebitsOnce(t *testing.T) {
	w, _, members := setup(t)
	form := members[0]
	grantRoots(t, w, "form", 1)
	gen, err := os.ReadFile(filepath.Join(form.leaf(), "gen"))
	if err != nil {
		t.Fatal(err)
	}
	key := randomHex(16)
	req := memberRequest(key, string(gen), form.keyDigest(), `{"to":["tax","lint"],"body":"lost?"}`)
	req.Verb = "ask"
	resp, data := plant(t, form.leaf(), req, true)
	if resp.Status != 0 {
		t.Fatalf("planted ask failed: %d %s", resp.Status, data)
	}
	var first domain.AskResult
	if err := json.Unmarshal([]byte(data), &first); err != nil {
		t.Fatal(err)
	}
	members[1].must("read", strconv.FormatInt(first.ID, 10))
	again := form.must("ask", "@lint", "@tax", "lost?", "--key", key)
	var replay domain.AskResult
	if err := json.Unmarshal([]byte(again.stdout), &replay); err != nil {
		t.Fatal(err)
	}
	if replay.ID != first.ID || replay.Exchange != first.Exchange || replay.DueAtMS != first.DueAtMS ||
		replay.Debit != first.Debit || replay.Remaining != first.Remaining {
		t.Fatalf("replay changed the committed send: first=%+v replay=%+v", first, replay)
	}
	states := make(map[string]string)
	for _, recipient := range replay.Recipients {
		states[recipient.Alias] = recipient.State
	}
	if len(replay.Recipients) != 2 || !reflect.DeepEqual(states, map[string]string{"tax": "picked_up", "lint": "stored"}) {
		t.Fatalf("delivery after tax read: %+v", replay.Recipients)
	}
	var asks, spent int
	db := w.db()
	if err := db.QueryRow(`SELECT COUNT(*) FROM message WHERE kind = 'ask'`).Scan(&asks); err != nil || asks != 1 {
		t.Fatalf("%d asks (%v)", asks, err)
	}
	if err := db.QueryRow(`SELECT spent FROM exchange`).Scan(&spent); err != nil || spent != 2 {
		t.Fatalf("spent %d (%v), want 2", spent, err)
	}
}
