//go:build e2e

package e2e

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/domain"
)

func TestAskPickupEvidenceAndThreadAnswer(t *testing.T) {
	w, owner, members := setup(t)
	sender, recipient := members[0], members[1]
	grantRoots(t, w, "form", 1)
	const question, answer = "Which column has the invoice date?", "Use the posting date."
	id := num(t, sender.must("ask", "@tax", question), "id")
	arg := strconv.FormatInt(id, 10)
	if out := recipient.mustPlain("who"); !slices.Contains(strings.Split(out, "\n"), "[crosshall] pending: "+arg) {
		t.Fatalf("missing pending request in plain output: %q", out)
	}

	receipts := func() []string {
		t.Helper()
		row := ledgerRecipients(t, owner.must("owner", "status", "--all"), id)["tax"]
		var got []string
		for _, item := range row["receipts"].([]any) {
			r := item.(map[string]any)
			got = append(got, fmt.Sprintf("%s/%s", r["stage"], r["evidence"]))
		}
		return got
	}
	if got := receipts(); !slices.Equal(got, []string{"stored/dispatcher"}) {
		t.Fatalf("before read: %v", got)
	}
	for i := 1; i <= 2; i++ {
		read := siblingMessages(t, recipient.must("read", arg))
		if len(read) != 1 || read[0].Body != question {
			t.Fatalf("read %d: %+v", i, read)
		}
		if got := receipts(); !slices.Equal(got, []string{"stored/dispatcher", "picked_up/cli"}) {
			t.Fatalf("after read %d: %v", i, got)
		}
	}

	replyID := num(t, recipient.must("reply", arg, answer), "id")
	if got := receipts(); !slices.Equal(got, []string{"stored/dispatcher", "picked_up/cli", "settled/cli"}) {
		t.Fatalf("after reply: %v", got)
	}
	var thread domain.ReadResult
	r := sender.must("read", arg)
	if err := json.Unmarshal([]byte(r.stdout), &thread); err != nil {
		t.Fatal(err)
	}
	if len(thread.Messages) != 1 || thread.Messages[0].ID != id || len(thread.Messages[0].Replies) != 1 {
		t.Fatalf("read of the original request returned %s", r.stdout)
	}
	reply := thread.Messages[0].Replies[0]
	if reply.ID != replyID || reply.InReplyTo == nil || *reply.InReplyTo != id || reply.Sender != "tax" || reply.Body != answer {
		t.Fatalf("wrong reply on request %d: %+v", id, reply)
	}
}

func TestFanoutAtomicityAndWaitingUntilRepliesAreRead(t *testing.T) {
	w, _, members := setup(t)
	sender, tax, lint := members[0], members[1], members[2]
	grantRoots(t, w, "form", 1)
	ask := sender.must("ask", "@tax", "@lint", "Check the invoice totals")
	id := num(t, ask, "id")
	arg := strconv.FormatInt(id, 10)

	db := w.db()
	type counters struct {
		messages, recipients, obligations, receipts int
		exchanges, spent, budgetEvents, operations  int
	}
	committed := func() counters {
		t.Helper()
		var n counters
		err := db.QueryRow(`SELECT (SELECT count(*) FROM message), (SELECT count(*) FROM recipient),
			(SELECT count(*) FROM obligation), (SELECT count(*) FROM receipt),
			(SELECT count(*) FROM exchange), (SELECT sum(spent) FROM exchange),
			(SELECT count(*) FROM budget_event), (SELECT count(*) FROM op)`).
			Scan(&n.messages, &n.recipients, &n.obligations, &n.receipts, &n.exchanges, &n.spent, &n.budgetEvents, &n.operations)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	before := committed()
	if before.messages != 1 || before.recipients != 2 || before.obligations != 2 || before.receipts != 2 ||
		before.exchanges != 1 || before.spent != 1 || num(t, ask, "debit") != 1 {
		t.Fatalf("two-target ask: %+v, response %s", before, ask.stdout)
	}
	bad := sender.run("ask", "@tax", "@lint", "@missing", "Another check")
	after := committed()
	if bad.exit != 3 || bad.obj["code"] != "unknown_alias" || after != before {
		t.Fatalf("invalid fan-out: exit %d, %s, before %+v after %+v", bad.exit, bad.stdout, before, after)
	}

	// Another member's open request must stay out of the sender's waiting list.
	tax.must("ask", "@lint", "Check your subtotal")
	checkWaiting := func(want map[string]string) {
		t.Helper()
		plain := sender.mustPlain("waiting")
		var waiting domain.WaitingResult
		if err := json.Unmarshal([]byte(sender.must("waiting").stdout), &waiting); err != nil {
			t.Fatal(err)
		}
		if len(want) == 0 {
			if len(waiting.Requests) != 0 || strings.TrimSpace(plain) != "No outstanding requests." {
				t.Fatalf("completed request still waiting: %+v / %s", waiting, plain)
			}
			return
		}
		if len(waiting.Requests) != 1 || waiting.Requests[0].ID != id || len(waiting.Requests[0].Recipients) != len(want) ||
			strings.Count(plain, " from @") != 1 || !strings.Contains(plain, "#"+arg+" from @form,") || strings.Count(plain, "\n  @") != len(want) {
			t.Fatalf("waiting returned the wrong audience or requests: %+v / %s", waiting, plain)
		}
		states := make(map[string]string)
		for _, row := range waiting.Requests[0].Recipients {
			state := row.State
			if state == "open" && row.PickedUpAtMS == nil {
				state = "stored"
			}
			if row.ReplyUnread {
				state += ", unread reply"
			}
			states[row.Alias] = state
		}
		if !maps.Equal(states, want) {
			t.Fatalf("JSON waiting: got %v, want %v", states, want)
		}
		for alias, state := range want {
			if !strings.Contains(plain, "\n  @"+alias+" "+state+"\n") {
				t.Fatalf("plain waiting lacks @%s %s: %q", alias, state, plain)
			}
		}
	}
	checkWaiting(map[string]string{"tax": "stored", "lint": "stored"})
	first := num(t, tax.must("reply", arg, "The tax matches."), "id")
	checkWaiting(map[string]string{"tax": "replied, unread reply", "lint": "stored"})
	second := num(t, lint.must("reply", arg, "--decline"), "id")
	checkWaiting(map[string]string{"tax": "replied, unread reply", "lint": "declined, unread reply"})
	sender.must("read", strconv.FormatInt(first, 10))
	checkWaiting(map[string]string{"tax": "replied", "lint": "declined, unread reply"})
	sender.must("read", strconv.FormatInt(second, 10))
	checkWaiting(nil)
}
