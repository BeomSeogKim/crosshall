package domain

import (
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
