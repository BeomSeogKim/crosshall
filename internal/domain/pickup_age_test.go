package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

// pickupWorld: a writer asks claude1, a Claude Code session in turn-1 of
// launch pid-1, which reads the request.
func pickupWorld(t *testing.T) (*fixture, session, session, int64) {
	t.Helper()
	f, writer := adapterChannel(t)
	wantOK(t, f.owner(BudgetGrant{Channel: "checkout", Alias: "writer", Roots: 5}, "writer-roots"))
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()
	f.hook(s, start("pid-1", "startup", t0))
	f.hook(s, prompt("pid-1", "turn-1", t0+1_000))
	id := decode[AskResult](t, f.run(writer, Ask{To: []string{"claude1"}, Body: "q"}, "")).ID
	wantOK(t, f.run(s, Read{IDs: []int64{id}}, "read-1"))
	return f, writer, s, id
}

func recipientOf(t *testing.T, requests []LedgerRequest, id int64) LedgerRecipient {
	t.Helper()
	for _, r := range requests {
		if r.ID == id && len(r.Recipients) == 1 {
			return r.Recipients[0]
		}
	}
	t.Fatalf("request #%d not in %+v", id, requests)
	return LedgerRecipient{}
}

func TestWaitingShowsHowLongAgoTheRequestWasPickedUp(t *testing.T) {
	f, writer, s, id := pickupWorld(t)
	f.hook(s, stop("pid-1", "turn-1", f.now.UnixMilli()+2_000))
	f.now = f.now.Add(2 * time.Hour)
	r := recipientOf(t, decode[WaitingResult](t, f.run(writer, Waiting{}, "")).Requests, id)
	w := decode[WaitingResult](t, f.run(writer, Waiting{}, ""))
	if r.PickedUpAtMS == nil || w.GeneratedAtMS-*r.PickedUpAtMS < 7_200_000 {
		t.Fatalf("picked up at %v, generated at %d", r.PickedUpAtMS, w.GeneratedAtMS)
	}
	if r.Observed == nil || r.Observed.Fact != FactTurnFinishing || r.Observed.Source != "claude" {
		t.Fatalf("observed %+v", r.Observed)
	}
	if seen := r.Observed.At(w.GeneratedAtMS); seen.AgeS < 7_190 || seen.Current {
		t.Fatalf("seen two hours on: %+v", seen)
	}
	// The same in status, for the asker.
	st := decode[StatusResult](t, f.run(writer, Status{}, ""))
	if r := recipientOf(t, st.Channels[0].Requests, id); r.PickedUpAtMS == nil || r.Observed == nil {
		t.Fatalf("status recipient %+v", r)
	}
}

// A settled request shows no observed fact: it no longer waits on anyone.
func TestASettledRecipientShowsNoObservedFact(t *testing.T) {
	f, writer, s, id := pickupWorld(t)
	wantOK(t, f.run(s, Reply{To: id, Body: "done"}, ""))
	st := decode[StatusResult](t, f.run(writer, Status{}, ""))
	if r := recipientOf(t, st.Channels[0].Requests, id); r.Observed != nil {
		t.Fatalf("settled recipient observed %+v", r.Observed)
	}
}

// A recipient without an adapter has no observed fact, and one that has not
// picked the request up has no pickup age.
func TestNoAdapterAndNoPickupLeaveTheirFieldsOut(t *testing.T) {
	f, writer := adapterChannel(t)
	wantOK(t, f.owner(BudgetGrant{Channel: "checkout", Alias: "writer", Roots: 5}, "writer-roots"))
	plain := f.newSession("codex")
	wantOK(t, f.join(plain, Join{Channel: "checkout", Alias: "plain"}, ""))
	id := decode[AskResult](t, f.run(writer, Ask{To: []string{"plain"}, Body: "q"}, "")).ID
	r := recipientOf(t, decode[WaitingResult](t, f.run(writer, Waiting{}, "")).Requests, id)
	if r.PickedUpAtMS != nil || r.Observed != nil {
		t.Fatalf("recipient %+v", r)
	}
}

// A taken alias tells a session that has never joined to ask the owner for
// a restore before joining under another alias, in the hint itself: a key
// that has joined can no longer be restored.
func TestATakenAliasPointsAtOwnerRestore(t *testing.T) {
	f, _ := adapterChannel(t)
	other := f.newSession("claude")
	r := f.join(other, Join{Channel: "checkout", Alias: "writer"}, "")
	var refusal struct{ Message, Hint string }
	if err := json.Unmarshal(r.Data, &refusal); err != nil || r.Code != core.CodeAliasTaken {
		t.Fatalf("%s %s", r.Code, r.Data)
	}
	restore := strings.Index(refusal.Hint, "crosshall owner restore")
	another := strings.Index(refusal.Hint, "--as <another alias>")
	if restore < 0 || another < restore || !strings.Contains(refusal.Hint, "new session") {
		t.Fatalf("hint %q", refusal.Hint)
	}
}

// A departed member coming back to a reused alias is already enrolled, so a
// restore does not apply: it keeps the plain retry under another alias.
func TestADepartedMemberKeepsTheRetryUnderAnotherAlias(t *testing.T) {
	f, writer := adapterChannel(t)
	wantOK(t, f.run(writer, Leave{}, ""))
	taker := f.newSession("claude")
	wantOK(t, f.join(taker, Join{Channel: "checkout", Alias: "writer"}, ""))
	r := f.join(writer, Join{Channel: "checkout"}, "")
	var refusal struct{ Message, Hint string }
	if err := json.Unmarshal(r.Data, &refusal); err != nil || r.Code != core.CodeAliasTaken {
		t.Fatalf("%s %s", r.Code, r.Data)
	}
	if strings.Contains(refusal.Message+refusal.Hint, "restore") || refusal.Hint != "crosshall join checkout --as <another alias>" {
		t.Fatalf("departed member refusal %+v", refusal)
	}
}
