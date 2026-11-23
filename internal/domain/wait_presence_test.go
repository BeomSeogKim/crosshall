package domain

import (
	"testing"
	"time"
)

const pullNext = "waits for its next crosshall command"

// waitPresenceChannel is a channel with a writer and a reader, both shell
// members (vendor route, no publish route).
func waitPresenceChannel(t *testing.T) (*fixture, session, session) {
	t.Helper()
	f := newFixture(t)
	wantOK(t, f.owner(CreateChannel{Name: "checkout"}, ""))
	return f, ledgerMember(t, f, "checkout", "writer"), ledgerMember(t, f, "checkout", "reader")
}

// poll runs one wait snapshot for member and returns its cursor for the next.
func poll(t *testing.T, f *fixture, member session, cmd Wait) Wait {
	t.Helper()
	r := decode[WaitSnapshot](t, actLedger(f, member, cmd, ""))
	cmd.Cursor = &r.Cursor
	return cmd
}

func askNext(t *testing.T, f *fixture, from session, key string) string {
	t.Helper()
	return decode[AskResult](t, actLedger(f, from, Ask{To: []string{"reader"}, Body: "work"}, key)).Recipients[0].Next
}

func noteNext(t *testing.T, f *fixture, from session, key string) string {
	t.Helper()
	return decode[NoteResult](t, actLedger(f, from, Note{To: []string{"reader"}, Body: "fyi"}, key)).Recipients[0].Next
}

func whoWaitRunning(t *testing.T, f *fixture, viewer session, alias string) bool {
	t.Helper()
	for _, m := range decode[WhoResult](t, f.who(viewer)).Members {
		if m.Alias == alias {
			return m.Wake.Wake == WakeWait
		}
	}
	t.Fatalf("who lists no @%s", alias)
	return false
}

func TestWaitPresenceIsFreshForThreeSeconds(t *testing.T) {
	f, writer, reader := waitPresenceChannel(t)
	if got := askNext(t, f, writer, "no-wait"); got != pullNext {
		t.Fatalf("no wait: %q", got)
	}
	if whoWaitRunning(t, f, writer, "reader") {
		t.Fatal("who shows a wait that never ran")
	}

	poll(t, f, reader, Wait{Existing: true}) // the first ask is unread, so this wait ends
	if got := askNext(t, f, writer, "after-ended-wait"); got != pullNext {
		t.Fatalf("a wait that returned work counts as running: %q", got)
	}
	wantOK(t, actLedger(f, reader, Read{}, "")) // pick up both asks
	poll(t, f, reader, Wait{Existing: true})
	if got := askNext(t, f, writer, "running"); got != WaitRunning {
		t.Fatalf("running wait: %q", got)
	}
	if !whoWaitRunning(t, f, writer, "reader") || whoWaitRunning(t, f, writer, "writer") {
		t.Fatal("who does not show exactly the reader's wait")
	}

	f.now = f.now.Add(waitFresh)
	if got := askNext(t, f, writer, "three-seconds"); got != WaitRunning {
		t.Fatalf("a poll 3 s old: %q", got)
	}
	wantOK(t, actLedger(f, reader, Read{}, ""))
	f.now = f.now.Add(time.Millisecond)
	if got := askNext(t, f, writer, "stale"); got != pullNext {
		t.Fatalf("a poll older than 3 s: %q", got)
	}
	if whoWaitRunning(t, f, writer, "reader") {
		t.Fatal("who shows a stale wait")
	}

	// A snapshot that returns work ends the wait and does not refresh it.
	wantOK(t, actLedger(f, reader, Read{}, ""))
	cmd := poll(t, f, reader, Wait{Existing: true})
	if got := askNext(t, f, writer, "ends-the-wait"); got != WaitRunning {
		t.Fatalf("a new wait: %q", got)
	}
	f.now = f.now.Add(time.Second)
	if r := decode[WaitSnapshot](t, actLedger(f, reader, cmd, "")); len(r.Unread) != 1 {
		t.Fatalf("the wait did not return the ask: %+v", r)
	}
	f.now = f.now.Add(500 * time.Millisecond) // the earlier poll is still fresh
	if whoWaitRunning(t, f, writer, "reader") {
		t.Fatal("a wait that returned work stays running")
	}
	if got := askNext(t, f, writer, "after-the-wait-returned"); got != pullNext {
		t.Fatalf("a wait that returned work: %q", got)
	}
}

func TestWaitPresenceFollowsTheWaitFilter(t *testing.T) {
	f, writer, reader := waitPresenceChannel(t)
	poll(t, f, reader, Wait{Existing: true, WorkOnly: true})
	if got := noteNext(t, f, writer, "note"); got != pullNext {
		t.Fatalf("a note to a --work-only wait: %q", got)
	}
	if got := askNext(t, f, writer, "ask"); got != WaitRunning {
		t.Fatalf("an ask to a --work-only wait: %q", got)
	}

	f.now = f.now.Add(time.Second)
	wantOK(t, actLedger(f, reader, Read{}, ""))
	poll(t, f, reader, Wait{Existing: true})
	if got := noteNext(t, f, writer, "note-default"); got != WaitRunning {
		t.Fatalf("a note to a default wait: %q", got)
	}
}

func TestWaitPresenceOnReplies(t *testing.T) {
	f, writer, reader := waitPresenceChannel(t)
	ask := decode[AskResult](t, actLedger(f, reader, Ask{To: []string{"writer"}, Body: "work"}, "ask"))
	wantOK(t, actLedger(f, writer, Read{}, ""))
	poll(t, f, reader, Wait{Existing: true, WorkOnly: true})
	reply := func(outcome, key string) string {
		t.Helper()
		return decode[ReplyResult](t, actLedger(f, writer, Reply{To: ask.ID, Body: "later", Outcome: outcome}, key)).Delivery.Next
	}
	if got := reply(OutcomeMoreTime, "more-time"); got != pullNext {
		t.Fatalf("a request for more time ends no wait: %q", got)
	}
	if got := reply("", "final"); got != WaitRunning {
		t.Fatalf("a final reply to the asker's wait: %q", got)
	}
}

func TestWaitPresenceIsLostWithTheDispatcher(t *testing.T) {
	f, writer, reader := waitPresenceChannel(t)
	poll(t, f, reader, Wait{Existing: true})
	f.svc = &Service{Clock: f.svc.Clock} // a restarted dispatcher
	if got := askNext(t, f, writer, "after-restart"); got != pullNext {
		t.Fatalf("after a restart: %q", got)
	}
}

func TestWaitPresenceIgnoresARefusedPoll(t *testing.T) {
	f, writer, reader := waitPresenceChannel(t)
	for name, cmd := range map[string]Wait{
		"usage":              {Any: true, WorkOnly: true},
		"membership changed": {Cursor: &WaitCursor{BindingID: 1 << 40}},
	} {
		if r := actLedger(f, reader, cmd, ""); r.Status == 0 {
			t.Fatalf("%s: the poll was accepted", name)
		}
		if got := askNext(t, f, writer, "refused "+name); got != pullNext {
			t.Fatalf("%s: a refused wait counts as running: %q", name, got)
		}
	}
}

func TestPublishNoticeQueuedWinsOverARunningWait(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	wantOK(t, p.run(p.reader, Read{}, "")) // the ask sent before the route
	poll(t, p.fixture, p.reader, Wait{Existing: true})
	if d := decode[AskResult](t, p.ask(p.writer, Ask{To: []string{"reader"}, Body: "work"}, "queued")).Recipients[0]; d.Next != "notice queued" {
		t.Fatalf("available route and a running wait: %+v", d)
	}
	wantOK(t, p.run(p.reader, Publish{}, "off"))
	if d := decode[AskResult](t, p.ask(p.writer, Ask{To: []string{"reader"}, Body: "work"}, "off")).Recipients[0]; d.Next != WaitRunning {
		t.Fatalf("notices off and a running wait: %+v", d)
	}
}

// F2: a wait snapshot carries Delivers, so a timeout tells a session the hall
// delivers to that it needs no wait.
func TestWaitSnapshotSaysWhenTheHallDelivers(t *testing.T) {
	p := newPublishFixture(t)
	delivers := func(cmd Wait) bool {
		t.Helper()
		return decode[WaitSnapshot](t, p.run(p.reader, cmd, "")).Delivers
	}
	if delivers(Wait{}) || delivers(Wait{Existing: true}) {
		t.Fatal("a candidate route delivers")
	}
	p.proveRoute()
	if !delivers(Wait{}) || !delivers(Wait{Existing: true, WorkOnly: true}) {
		t.Fatal("an available route does not deliver")
	}
	wantOK(t, p.run(p.reader, Publish{}, "off"))
	if delivers(Wait{}) {
		t.Fatal("a member that switched notices off delivers")
	}
}
