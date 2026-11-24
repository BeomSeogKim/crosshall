package domain

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

// wakeFixture is a publish fixture whose reader has a prompt line.
func wakeFixture(t *testing.T) *publishFixture {
	t.Helper()
	p := newPublishFixture(t)
	p.svc.HasPromptLine = func(adapter, revision string) bool {
		return adapter == testProfile.Adapter && revision == testProfile.ManifestRev
	}
	return p
}

// sendTo asks the reader and checks that the send result, who and waiting
// say the same thing about it, and that the send was counted once.
func (p *publishFixture) sendTo(step string, want Wake) {
	p.t.Helper()
	p.now = p.now.Add(time.Millisecond)
	before := p.count(`SELECT COUNT(*) FROM event WHERE kind = 'send_wake'`)
	r := p.ask(p.writer, Ask{To: []string{"reader"}, New: true, Body: "work"}, "")
	wantOK(p.t, r)
	sent := decode[AskResult](p.t, r)
	if got := sent.Recipients[0]; got.State != "stored" || got.Wake != want {
		p.t.Fatalf("%s: send result %+v, want %+v", step, got, want)
	}
	if got := p.count(`SELECT COUNT(*) FROM event WHERE kind = 'send_wake' AND json_extract(detail, '$.wake') = ?`, want.Wake); got < 1 ||
		p.count(`SELECT COUNT(*) FROM event WHERE kind = 'send_wake'`) != before+1 {
		p.t.Fatalf("%s: the send was not counted once as %s", step, want.Wake)
	}
	member := want
	member.SeenAt = ""
	if want.Why == WhyKind {
		member = Wake{Wake: WakeNotice}
	}
	if got := p.memberRow(p.writer, "reader").Wake; got != member {
		p.t.Fatalf("%s: who says %+v, want %+v", step, got, member)
	}
	for _, req := range decode[WaitingResult](p.t, p.run(p.writer, Waiting{}, "")).Requests {
		if req.ID == sent.ID {
			if got := req.Recipients[0].Wake; got != want {
				p.t.Fatalf("%s: waiting says %+v, want %+v", step, got, want)
			}
			return
		}
	}
	p.t.Fatalf("%s: #%d not in waiting", step, sent.ID)
}

func notWoken(why string) Wake {
	return Wake{Wake: WakeNone, Why: why, SeenAt: SeenAtPrompt}
}

// Each situation a sender used to be told "waits for its next crosshall
// command" for now has its own fixed word.
func TestSendResultSaysWhatWillTellEachRecipient(t *testing.T) {
	t.Run("unproven route", func(t *testing.T) {
		p := wakeFixture(t)
		p.sendTo("candidate route", notWoken(WhyUnproven))
	})
	t.Run("notice", func(t *testing.T) {
		p := wakeFixture(t)
		p.proveRoute()
		p.sendTo("available route", Wake{Wake: WakeNotice})
	})
	t.Run("held", func(t *testing.T) {
		p := wakeFixture(t)
		p.proveRoute()
		p.turnEndAsk(p.writer, "reader", "first")
		id := p.reserve()
		p.begin(id)
		p.resolve(id, PublishSubmitted)
		p.sendTo("held route", notWoken(WhyHeld))
	})
	t.Run("suspended", func(t *testing.T) {
		p := wakeFixture(t)
		p.proveRoute()
		p.observed("unknown")
		until := p.publishState().UntilMS
		if until == 0 {
			t.Fatal("the first suspension has no end")
		}
		w := notWoken(WhySuspended)
		w.UntilMS = until
		p.sendTo("timed suspension", w)
	})
	t.Run("off", func(t *testing.T) {
		p := wakeFixture(t)
		p.proveRoute()
		wantOK(t, p.run(p.reader, Publish{}, "off"))
		p.sendTo("member off", notWoken(WhyOff))
		wantOK(t, p.run(p.reader, Publish{On: true}, "on"))
		p.mutate(`UPDATE binding SET dnd = 1 WHERE id = ?`, p.binding.ID)
		// Do not disturb also takes the prompt line away.
		p.sendTo("do not disturb", Wake{Wake: WakeNone, Why: WhyOff, SeenAt: SeenAtCommand})
	})
	t.Run("capped", func(t *testing.T) {
		p := wakeFixture(t)
		p.proveRoute()
		p.mutate(`WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < ?)
			INSERT INTO publish_attempt (binding_id, principal_id, binding_epoch, generation, adapter, session_ref, launch_ref,
				policy_epoch, process_pid, process_start_ms, state, reserved_at_ms, submitting_at_ms, resolved_at_ms, observed_at_ms, observation)
			SELECT binding_id, principal_id, binding_epoch, generation, adapter, session_ref, launch_ref,
				policy_epoch, 42, ?, 'submitted', ?, ?, ?, ?, 'own_turn'
			FROM publish_route CROSS JOIN seq WHERE binding_id = ?`,
			store.PublishHourlyCap, p.queue.StartMS, p.now.UnixMilli(), p.now.UnixMilli(), p.now.UnixMilli(), p.now.UnixMilli(), p.binding.ID)
		w := notWoken(WhyCapped)
		w.UntilMS = p.now.Add(time.Hour).UnixMilli()
		p.sendTo("hourly cap", w)
	})
	t.Run("no sender for the adapter", func(t *testing.T) {
		p := wakeFixture(t)
		p.policy("agy", true, 1)
		p.mutate(`UPDATE publish_route SET adapter = 'agy' WHERE binding_id = ?`, p.binding.ID)
		w := notWoken(WhyNoRoute)
		w.Adapter = "agy"
		p.sendTo("agy route", w)
	})
	for _, mode := range []string{"in_process", "unverified"} {
		t.Run("codex "+mode, func(t *testing.T) {
			p := wakeFixture(t)
			p.policy("codex", true, 1)
			p.mutate(`UPDATE publish_route SET adapter = 'codex', mode = ? WHERE binding_id = ?`, mode, p.binding.ID)
			w := notWoken(WhyNoRoute)
			w.Adapter = "codex"
			p.sendTo("codex route", w)
		})
	}
	t.Run("no route", func(t *testing.T) {
		p := wakeFixture(t)
		p.mutate(`DELETE FROM publish_route WHERE binding_id = ?`, p.binding.ID)
		p.sendTo("no route", notWoken(WhyNoRoute))
	})
}

// A member with no route and no prompt line sees new work at its next
// command; a note to a member the hall can wake starts no turn.
func TestNotesAndRepliesSayWhatWillTellTheirRecipient(t *testing.T) {
	p := wakeFixture(t)
	p.proveRoute()
	counted := p.count(`SELECT COUNT(*) FROM event WHERE kind = 'send_wake'`)
	note := decode[NoteResult](t, p.run(p.writer, Note{To: []string{"reader"}, Body: "fyi"}, "note"))
	if got, want := note.Recipients[0].Wake, notWoken(WhyKind); got != want {
		t.Fatalf("a note: %+v, want %+v", got, want)
	}
	if n := p.count(`SELECT COUNT(*) FROM event WHERE kind = 'send_wake'`); n != counted {
		t.Fatalf("a note was counted: %d events, %d before", n, counted)
	}

	ask := p.turnEndAsk(p.writer, "reader", "ask")
	wantOK(t, p.run(p.reader, Read{IDs: []int64{ask}}, "read"))
	none := `SELECT COUNT(*) FROM event WHERE kind = 'send_wake' AND json_extract(detail, '$.wake') = 'none'`
	counted = p.count(none)
	more := decode[ReplyResult](t, p.reply(p.reader, Reply{To: ask, Outcome: OutcomeMoreTime, Body: "soon"}, "more"))
	answer := decode[ReplyResult](t, p.reply(p.reader, Reply{To: ask, Body: "done"}, "answer"))
	// The writer is a Codex member that joined by hand: no route, no prompt line.
	want := Wake{Wake: WakeNone, Why: WhyNoRoute, SeenAt: SeenAtCommand}
	if more.Delivery.Wake != want || answer.Delivery.Wake != want {
		t.Fatalf("replies to the writer: %+v and %+v, want %+v", more.Delivery.Wake, answer.Delivery.Wake, want)
	}
	if n := p.count(none); n != counted+1 {
		t.Fatalf("the final reply was counted %d times, the more-time reply must not be", n)
	}
}

// A replay shows what is true now: a request a notice offered stays a
// notice after the notice was submitted, and a recipient that left is held.
func TestSendReplayShowsTheCurrentWake(t *testing.T) {
	p := wakeFixture(t)
	p.proveRoute()
	counted := p.count(`SELECT COUNT(*) FROM event WHERE kind = 'send_wake'`)
	send := func(key string) Delivery {
		t.Helper()
		return decode[AskResult](t, p.ask(p.writer, Ask{To: []string{"reader"}, Body: "work"}, key)).Recipients[0]
	}
	if d := send("queued"); d.Wake != (Wake{Wake: WakeNotice}) || d.Next != "notice queued" {
		t.Fatalf("available route: %+v", d)
	}
	id := p.reserve()
	p.begin(id)
	p.resolve(id, PublishSubmitted)
	if d := send("queued"); d.Wake != (Wake{Wake: WakeNotice}) {
		t.Fatalf("replay after the notice was submitted: %+v", d)
	}
	if n := p.count(`SELECT COUNT(*) FROM event WHERE kind = 'send_wake'`); n != counted+1 {
		t.Fatalf("a replay was counted: %d events, %d before the first send", n, counted)
	}
	wantOK(t, p.run(p.reader, Leave{}, "leave"))
	if d := send("queued"); d.Wake != (Wake{}) || d.Next != "held until it rejoins" {
		t.Fatalf("recipient left: %+v", d)
	}
}

// waitingRecipient is the reader's row of request id in the writer's waiting.
func (p *publishFixture) waitingRecipient(id int64) LedgerRecipient {
	p.t.Helper()
	for _, req := range decode[WaitingResult](p.t, p.run(p.writer, Waiting{}, "")).Requests {
		if req.ID == id {
			return req.Recipients[0]
		}
	}
	p.t.Fatalf("#%d not in waiting", id)
	return LedgerRecipient{}
}

// Once a request has a notice of its own, waiting shows that notice and no
// wake words: a spent notice is not a queued one.
func TestWaitingShowsNoWakeForARequestWithItsOwnNotice(t *testing.T) {
	p := wakeFixture(t)
	p.proveRoute()
	id := p.turnEndAsk(p.writer, "reader", "work")
	if r := p.waitingRecipient(id); r.Wake != (Wake{Wake: WakeNotice}) || r.Notice != nil {
		t.Fatalf("before the notice: %+v", r)
	}
	attempt := p.reserve()
	p.begin(attempt)
	p.resolve(attempt, PublishSubmitted)
	p.mutate(`UPDATE publish_attempt SET observed_at_ms = ?, observation = 'unknown' WHERE id = ?`, p.now.UnixMilli(), attempt)
	p.update(func(ctx context.Context, tx store.Tx) error {
		return suspendPublish(ctx, tx, attempt, "unknown", p.now)
	})
	if s := p.publishState(); s.State != "suspended" {
		t.Fatalf("state %+v, want suspended", s)
	}
	if r := p.waitingRecipient(id); r.Wake != (Wake{}) || r.Notice == nil || r.Notice.Observation != "unknown" {
		t.Fatalf("after an unknown notice: %+v", r)
	}
	if w := p.memberRow(p.writer, "reader").Wake; w.Wake != WakeNone || w.Why != WhySuspended {
		t.Fatalf("who: %+v", w)
	}
}

// A recipient that picked a request up, or asked for more time, has no wake
// words in waiting.
func TestWaitingShowsNoWakeOnceARequestIsPickedUp(t *testing.T) {
	p := wakeFixture(t)
	picked := p.turnEndAsk(p.writer, "reader", "picked")
	wantOK(t, p.run(p.reader, Read{IDs: []int64{picked}}, "read"))
	if r := p.waitingRecipient(picked); r.Wake != (Wake{}) || r.PickedUpAtMS == nil {
		t.Fatalf("picked up: %+v", r)
	}
	// A request for more time can come before the request is read.
	unread := p.turnEndAsk(p.writer, "reader", "unread")
	wantOK(t, p.reply(p.reader, Reply{To: unread, Outcome: OutcomeMoreTime, Body: "soon"}, "more"))
	if r := p.waitingRecipient(unread); r.Wake != (Wake{}) || r.MoreTimeAtMS == nil || r.PickedUpAtMS != nil {
		t.Fatalf("more time before reading: %+v", r)
	}
}

// A notice wins over a running wait in who, as in a send.
func TestWhoPrefersANoticeOverARunningWait(t *testing.T) {
	p := wakeFixture(t)
	wantOK(t, p.run(p.reader, Wait{}, ""))
	if w := p.memberRow(p.writer, "reader").Wake; w != (Wake{Wake: WakeWait}) {
		t.Fatalf("a running wait on an unproven route: %+v", w)
	}
	p.proveRoute()
	wantOK(t, p.run(p.reader, Wait{}, ""))
	if w := p.memberRow(p.writer, "reader").Wake; w != (Wake{Wake: WakeNotice}) {
		t.Fatalf("a running wait on an available route: %+v", w)
	}
}

// A request to a member in a turn whose end the turn-end check will offer it
// at says so, and the Stop of that turn does offer it.
func TestARequestDuringACheckedTurnIsSeenWhenTheTurnEnds(t *testing.T) {
	f, reader, writer := turnEndFixture(t, "launch")
	f.svc.HasPromptLine = func(adapter, revision string) bool { return adapter == "claude" }
	send := func(key string) Delivery {
		t.Helper()
		return decode[AskResult](t, f.ask(writer, Ask{To: []string{"reader"}, New: true, Body: "work"}, key)).Recipients[0]
	}
	if d := send("not-covered"); d.Wake.Wake != WakeNone || d.SeenAt != SeenAtPrompt {
		t.Fatalf("no turn-end evidence: %+v", d)
	}
	wantOK(t, f.owner(turnEndEvidence("tested"), "evidence"))
	d := send("in-turn")
	if d.Wake.Wake != WakeNone || d.SeenAt != SeenAtTurnEnd {
		t.Fatalf("in a checked turn: %+v", d)
	}
	note := decode[NoteResult](t, f.run(writer, Note{To: []string{"reader"}, Body: "fyi"}, "note")).Recipients[0]
	if note.SeenAt != SeenAtPrompt {
		t.Fatalf("a note, which the check never offers: %+v", note)
	}
	exec(t, f, `UPDATE adapter_policy SET turn_end = 0 WHERE adapter = 'claude'`)
	if d := send("switched-off"); d.SeenAt != SeenAtPrompt {
		t.Fatalf("turn-end check off: %+v", d)
	}
	exec(t, f, `UPDATE adapter_policy SET turn_end = 1 WHERE adapter = 'claude'`)

	got := f.hook(reader, stop("launch", "turn-1", f.now.UnixMilli()+1))
	if !strings.Contains(got.TurnEndLine, "[crosshall] pending:") {
		t.Fatalf("the turn's end offered nothing: %+v", got)
	}
	// The turn had its check; the next request waits for the next prompt.
	if d := send("after-the-check"); d.SeenAt != SeenAtPrompt {
		t.Fatalf("after the turn's check: %+v", d)
	}
	f.hook(reader, prompt("launch", "turn-2", f.now.UnixMilli()+2))
	if d := send("next-turn"); d.SeenAt != SeenAtTurnEnd {
		t.Fatalf("in the next checked turn: %+v", d)
	}
	// Nothing follows an answered permission dialog: the turn shows that
	// stage until its Stop, which still offers the request.
	f.hook(reader, event("launch", FactApprovalRequested, "turn-2:approval", "turn-2", f.now.UnixMilli()+3, false))
	if d := send("after-approval"); d.SeenAt != SeenAtTurnEnd {
		t.Fatalf("after a permission request in the turn: %+v", d)
	}
	if got := f.hook(reader, stop("launch", "turn-2", f.now.UnixMilli()+4)); !strings.Contains(got.TurnEndLine, "[crosshall] pending:") {
		t.Fatalf("the turn's end after a permission request offered nothing: %+v", got)
	}
	f.hook(reader, prompt("launch", "turn-3", f.now.UnixMilli()+5))
	// A prompt past its cap is not a running turn any more.
	f.now = f.now.Add(31 * time.Minute)
	if d := send("stale-prompt"); d.SeenAt != SeenAtPrompt {
		t.Fatalf("a prompt 31 minutes old: %+v", d)
	}
}

func exec(t *testing.T, f *fixture, statement string) {
	t.Helper()
	if _, err := f.db.Exec(statement); err != nil {
		t.Fatal(err)
	}
}

// A request a turn end or a prompt line already showed, and that the member
// has not read, says when it was shown in waiting instead of where it will
// be seen: each request is shown once.
func TestWaitingSaysWhenAnUnreadRequestWasShown(t *testing.T) {
	f, reader, writer := turnEndFixture(t, "launch")
	f.svc.HasPromptLine = func(adapter, revision string) bool { return adapter == "claude" }
	wantOK(t, f.owner(turnEndEvidence("tested"), "evidence"))
	row := func(id int64) LedgerRecipient {
		t.Helper()
		for _, req := range decode[WaitingResult](t, f.run(writer, Waiting{}, "")).Requests {
			if req.ID == id {
				return req.Recipients[0]
			}
		}
		t.Fatalf("#%d not in waiting", id)
		return LedgerRecipient{}
	}
	atTurnEnd := f.turnEndAsk(writer, "reader", "at-turn-end")
	if r := row(atTurnEnd); r.SeenAt != SeenAtTurnEnd || r.ShownAtMS != nil {
		t.Fatalf("before the turn ends: %+v", r)
	}
	stopAt := f.now.UnixMilli() + 1
	if got := f.hook(reader, stop("launch", "turn-1", stopAt)); !strings.Contains(got.TurnEndLine, "pending:") {
		t.Fatalf("the turn's end offered nothing: %+v", got)
	}
	if r := row(atTurnEnd); r.Wake.Wake != WakeNone || r.SeenAt != "" || r.ShownAtMS == nil || *r.ShownAtMS != f.now.UnixMilli() {
		t.Fatalf("after the turn end showed it: %+v", r)
	}

	atPrompt := f.turnEndAsk(writer, "reader", "at-prompt")
	if r := row(atPrompt); r.SeenAt != SeenAtPrompt || r.ShownAtMS != nil {
		t.Fatalf("before the next prompt: %+v", r)
	}
	if got := f.hook(reader, prompt("launch", "turn-2", f.now.UnixMilli()+2)); !strings.Contains(got.PendingLine, "pending:") {
		t.Fatalf("the prompt showed nothing: %+v", got)
	}
	if r := row(atPrompt); r.SeenAt != "" || r.ShownAtMS == nil {
		t.Fatalf("after the prompt line showed it: %+v", r)
	}
	wantOK(t, f.run(reader, Wait{}, ""))
	if r := row(atPrompt); r.Wake.Wake != WakeNone || r.ShownAtMS == nil {
		t.Fatalf("a request behind the running wait's cursor: %+v", r)
	}
	duringWait := f.turnEndAsk(writer, "reader", "during-wait")
	if got := f.hook(reader, prompt("launch", "turn-3", f.now.UnixMilli()+2)); !strings.Contains(got.PendingLine, "pending:") {
		t.Fatalf("the next prompt showed nothing: %+v", got)
	}
	if r := row(duringWait); r.Wake != (Wake{Wake: WakeWait}) || r.ShownAtMS != nil {
		t.Fatalf("a shown request above the running wait's cursor: %+v", r)
	}
	f.svc.waits.forget(bindingIDOf(t, f, f.principalOf(reader)))
	wantOK(t, f.run(reader, Read{IDs: []int64{atPrompt}}, "read"))
	if r := row(atPrompt); r.Wake != (Wake{}) || r.ShownAtMS != nil {
		t.Fatalf("after reading: %+v", r)
	}
}

// A turn end offers at most OfferCap requests: the one past the cap is not
// seen when the turn ends, and its send line says where it is seen.
func TestARequestPastTheTurnEndCapIsNotSeenWhenTheTurnEnds(t *testing.T) {
	f, _, writer := turnEndFixture(t, "launch")
	f.svc.HasPromptLine = func(adapter, revision string) bool { return adapter == "claude" }
	wantOK(t, f.owner(turnEndEvidence("tested"), "evidence"))
	var d Delivery
	for i := range OfferCap + 1 {
		d = decode[AskResult](t, f.ask(writer, Ask{To: []string{"reader"}, New: true, Body: "work"}, fmt.Sprintf("ask-%d", i))).Recipients[0]
		if i < OfferCap && d.SeenAt != SeenAtTurnEnd {
			t.Fatalf("request %d inside the cap: %+v", i+1, d)
		}
	}
	if d.Wake.Wake != WakeNone || d.SeenAt != SeenAtPrompt {
		t.Fatalf("the request past the cap: %+v", d)
	}
}

func TestOpenTurnDefersTheSendersNoticePrediction(t *testing.T) {
	p := wakeFixture(t)
	p.proveRoute()
	wantOK(t, p.run(p.reader, Read{}, "initial"))
	wantOK(t, p.owner(turnEndEvidence("tested"), "evidence"))
	p.hook(p.reader, prompt(publishTestLaunch, "running", p.now.UnixMilli()+1))
	send := func(key string, want Wake, next string) int64 {
		t.Helper()
		sent := decode[AskResult](t, p.ask(p.writer, Ask{To: []string{"reader"}, New: true, Body: "work"}, key))
		d := sent.Recipients[0]
		if d.Wake != want || d.Next != next {
			t.Fatalf("%s send = %+v, want %+v", key, d, want)
		}
		if row := p.waitingRecipient(sent.ID); row.Wake != want || row.Notice != nil {
			t.Fatalf("%s waiting = %+v, want %+v", key, row, want)
		}
		if n := p.count(`SELECT COUNT(*) FROM event WHERE kind = 'send_wake' AND subject = ?
			AND json_extract(detail, '$.wake') = ?`, fmt.Sprintf("message:%d", sent.ID), want.Wake); n != 1 {
			t.Fatalf("%s recorded %d events as %s", key, n, want.Wake)
		}
		return sent.ID
	}
	poll(t, p.fixture, p.reader, Wait{Existing: true})
	first := send("waiting", Wake{Wake: WakeWait}, WaitRunning)
	p.svc.waits.forget(p.binding.ID)
	second := send("checked-stop", Wake{Wake: WakeNone, Why: WhyTurnOpen, SeenAt: SeenAtTurnEnd}, "at its turn-end check")
	for i := 2; i < OfferCap; i++ {
		p.turnEndAsk(p.writer, "reader", fmt.Sprintf("queued-%d", i))
	}
	last := send("past-stop-cap", Wake{Wake: WakeNone, Why: WhyTurnOpen}, "the hall checks again after its turn")
	if row := p.ownerRow("reader").UnreadNotWoken; row != nil {
		t.Fatalf("owner counted work deferred to Stop or publish as stranded: %+v", row)
	}
	if id := p.reserve(); id != 0 {
		t.Fatalf("reserved %d during a reported turn", id)
	}
	line := p.hook(p.reader, stop(publishTestLaunch, "running", p.now.UnixMilli()+1)).TurnEndLine
	if !strings.HasPrefix(line, pendingLine(first, second)+" ") {
		t.Fatalf("checked Stop returned %q", line)
	}
	if row := p.waitingRecipient(last); row.Wake != (Wake{Wake: WakeNotice}) {
		t.Fatalf("eligible after Stop: %+v", row)
	}
}

func TestSendReplayAfterReservedRouteSwitchesOff(t *testing.T) {
	p := wakeFixture(t)
	p.proveRoute()
	send := func() Delivery {
		t.Helper()
		r := p.ask(p.writer, Ask{To: []string{"reader"}, Body: "work"}, "replay")
		return decode[AskResult](t, r).Recipients[0]
	}
	if d := send(); d.Wake.Wake != WakeNotice || p.reserve() == 0 {
		t.Fatal("idle route did not reserve")
	}
	wantOK(t, p.run(p.reader, Publish{}, "off"))
	if got := send().Wake; got != notWoken(WhyOff) {
		t.Fatalf("after the switch changed, send replay = %+v, want notices off", got)
	}
}

func TestSendReplayDistinguishesReservedAndSubmittedNoticesDuringATurn(t *testing.T) {
	for _, submit := range []bool{false, true} {
		t.Run(fmt.Sprintf("submitted=%t", submit), func(t *testing.T) {
			p := wakeFixture(t)
			p.offerReceipts = true
			p.proveRoute()
			wantOK(t, p.run(p.reader, Read{}, "initial"))
			poll(t, p.fixture, p.reader, Wait{Existing: true})
			send := func() AskResult {
				t.Helper()
				return decode[AskResult](t, p.ask(p.writer, Ask{To: []string{"reader"}, Body: "work"}, "replay"))
			}
			id := send().ID
			attempt := p.reserve()
			if attempt == 0 {
				t.Fatal("idle recipient did not reserve a notice")
			}
			if submit {
				p.begin(attempt)
				p.resolve(attempt, PublishSubmitted)
			}
			p.hook(p.reader, prompt(publishTestLaunch, "running", p.now.UnixMilli()+1))
			d := send().Recipients[0]
			row := p.waitingRecipient(id)
			if submit {
				if d.Wake.Wake != WakeNotice || row.Notice == nil || row.Notice.State != "submitted" || row.Wake != (Wake{}) {
					t.Fatalf("submitted notice: send %+v, waiting %+v", d, row)
				}
				return
			}
			if d.Wake.Wake != WakeWait || row.Wake.Wake != WakeWait || row.Notice == nil || row.Notice.State != "reserved" {
				t.Fatalf("unsubmitted reservation: send %+v, waiting %+v", d, row)
			}
			p.svc.waits.forget(p.binding.ID)
			row = p.waitingRecipient(id)
			if row.Wake != (Wake{Wake: WakeNone, Why: WhyTurnOpen}) || row.ShownAtMS != nil {
				t.Fatalf("a reservation alone cannot show the request: %+v", row)
			}
		})
	}
}

func TestFinalReplyToAnOpenTurnReportsDeferredNotice(t *testing.T) {
	p := wakeFixture(t)
	p.proveRoute()
	wantOK(t, p.owner(turnEndEvidence("tested"), "evidence"))
	request := p.turnEndAsk(p.reader, "writer", "request")
	wantOK(t, p.run(p.writer, Read{IDs: []int64{request}}, "read"))
	p.hook(p.reader, prompt(publishTestLaunch, "busy", p.now.UnixMilli()+1))
	reply := decode[ReplyResult](t, p.reply(p.writer, Reply{To: request, Outcome: OutcomeAnswer, Body: "done"}, "answer"))
	if reply.Delivery.Wake != (Wake{Wake: WakeNone, Why: WhyTurnOpen}) || reply.Delivery.Next != "the hall checks again after its turn" {
		t.Fatalf("final reply is outside the ask-only Stop check: %+v", reply.Delivery)
	}
	if n := p.count(`SELECT COUNT(*) FROM event WHERE kind = 'send_wake' AND subject = ?
		AND json_extract(detail, '$.wake') = 'none' AND json_extract(detail, '$.why') = 'turn_open'`, fmt.Sprintf("message:%d", reply.ID)); n != 1 {
		t.Fatal("deferred final reply was not counted as wake=none")
	}
}
