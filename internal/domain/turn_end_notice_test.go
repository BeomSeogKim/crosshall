package domain

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func turnEndEvidence(level string) AdapterEvidence {
	e := evidenceFixture()
	e.Results = []EvidenceResult{{Cap: "turn_end", Level: level, Check: "one bounded turn-end continuation"}}
	return e
}

func turnEndFixture(t *testing.T, launch string) (*fixture, session, session) {
	t.Helper()
	f, writer := adapterChannel(t)
	reader := f.newSession("claude")
	wantOK(t, f.join(reader, Join{Channel: "checkout", Alias: "reader"}, "join-reader"))
	f.hook(reader, start(launch, "startup", f.now.UnixMilli()))
	f.hook(reader, prompt(launch, "turn-1", f.now.UnixMilli()+1))

	return f, reader, writer
}

func (f *fixture) turnEndAsk(writer session, alias, key string) int64 {
	f.t.Helper()
	r := f.ask(writer, Ask{To: []string{alias}, New: true, Body: "fixture body that must never appear in a hook"}, key)
	wantOK(f.t, r)
	return decode[AskResult](f.t, r).ID
}

func TestTurnEndNoticeClaimsInboxAsksWithoutPickup(t *testing.T) {
	f, reader, writer := turnEndFixture(t, "launch")
	wantOK(t, f.owner(turnEndEvidence("tested"), "evidence"))

	own := f.turnEndAsk(reader, "writer", "outgoing")
	wantOK(t, f.run(writer, Read{IDs: []int64{own}}, "writer-read"))
	reply := f.reply(writer, Reply{To: own, Body: "fixture answer", Outcome: OutcomeAnswer}, "answer")
	wantOK(t, reply)
	first := f.turnEndAsk(writer, "reader", "first")
	pickedUp := f.turnEndAsk(writer, "reader", "picked-up")
	wantOK(t, f.run(reader, Read{IDs: []int64{pickedUp}}, "read"))
	// A request the owner cancelled before it was read is listed apart and
	// never continues a turn.
	second := f.turnEndAsk(writer, "reader", "second")
	wantOK(t, f.owner(Cancel{Channel: "checkout", ID: second}, "cancel"))
	f.noteTo(writer, "reader", 1)
	pid := f.principalOf(reader)

	// A prompt-boundary offer does not clear or spend an unread ask.
	f.hook(reader, prompt("launch", "turn-2", f.now.UnixMilli()+1))
	stopAt := f.now.UnixMilli() + 1
	cmd := stop("launch", "turn-2", stopAt)
	got := f.hook(reader, cmd)
	if got.TurnEndLine != fmt.Sprintf("[crosshall] pending: %d", first) || got.Ack != nil || got.PendingLine != "" || got.ReminderText != "" {
		t.Fatalf("turn-end result = %+v, want only the unread ask %d", got, first)
	}
	after := decode[InboxResult](t, f.run(reader, Inbox{}, ""))
	unreadAsks := []int64{}
	for _, item := range after.Unread {
		if item.Kind == "ask" {
			unreadAsks = append(unreadAsks, item.ID)
		}
	}
	if !slices.Equal(unreadAsks, []int64{first}) || len(after.Cancelled) != 1 || after.Cancelled[0].ID != second {
		t.Fatalf("unread inbox asks changed: %v", unreadAsks)
	}
	if n := f.count(`SELECT COUNT(*) FROM receipt WHERE stage = 'picked_up' AND principal_id = ?`, pid); n != 1 {
		t.Fatalf("claim added pickup receipts: %d", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM announcement WHERE via = 'turn_end' AND principal_id = ? AND message_id IN (?, ?)`, pid, first, second); n != 1 {
		t.Fatalf("claimed %d selected inbox asks, want 1", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM announcement`); n != 1 {
		t.Fatalf("claimed %d items, want only the unread ask", n)
	}
	if r := f.hook(reader, cmd); r.TurnEndLine != "" {
		t.Fatalf("replayed Stop printed %q", r.TurnEndLine)
	}
	// A second distinct Stop in that turn may not spend another claim.
	third := f.turnEndAsk(writer, "reader", "third")
	secondStop := event("launch", FactTurnFinishing, "distinct-stop", "turn-2", f.now.UnixMilli()+1, true)
	if r := f.hook(reader, secondStop); r.TurnEndLine != "" {
		t.Fatalf("second Stop in the turn printed %q", r.TurnEndLine)
	}
	f.hook(reader, prompt("launch", "turn-3", f.now.UnixMilli()+1))
	r := f.hook(reader, stop("launch", "turn-3", f.now.UnixMilli()+1))
	if r.TurnEndLine != fmt.Sprintf("[crosshall] pending: %d", third) {
		t.Fatalf("the next turn did not claim ask %d: %+v", third, r)
	}
	if n := f.count(`SELECT COUNT(*) FROM announcement`); n != 2 {
		t.Fatalf("a later turn failed to keep per-recipient deduplication: %d rows", n)
	}
	f.hook(reader, prompt("launch", "turn-4", f.now.UnixMilli()+1))
	r = f.hook(reader, stop("launch", "turn-4", f.now.UnixMilli()+1))
	if r.TurnEndLine != "" || f.count(`SELECT COUNT(*) FROM turn_end_notice`) != 2 {
		t.Fatalf("an empty evaluation spent a turn claim: %+v", r)
	}
}

func TestTurnEndNoticeGuards(t *testing.T) {
	cases := []struct {
		name   string
		before func(*fixture, session, *HookEvent, *AdapterEvidence)
	}{
		{name: "one-way Stop", before: func(_ *fixture, _ session, c *HookEvent, _ *AdapterEvidence) { c.WantOutput = false }},
		{name: "native continuation underway", before: func(_ *fixture, _ session, c *HookEvent, _ *AdapterEvidence) { c.ContinuationActive = true }},
		{name: "policy off", before: func(f *fixture, _ session, _ *HookEvent, _ *AdapterEvidence) { f.policy("claude", false, 1) }},
		{name: "stale policy epoch", before: func(f *fixture, _ session, _ *HookEvent, _ *AdapterEvidence) { f.policy("claude", true, 2) }},
		{name: "unknown turn", before: func(_ *fixture, _ session, c *HookEvent, _ *AdapterEvidence) { c.TurnRef = "unknown" }},
		{name: "callback predates its turn", before: func(_ *fixture, _ session, c *HookEvent, _ *AdapterEvidence) { c.HookAtMS -= 2 }},
		{name: "superseded turn", before: func(f *fixture, reader session, c *HookEvent, _ *AdapterEvidence) {
			f.hook(reader, prompt(c.LaunchRef, "new-turn", c.HookAtMS))
		}},
		{name: "stale launch", before: func(f *fixture, reader session, c *HookEvent, _ *AdapterEvidence) {
			f.hook(reader, start("new-launch", "startup", c.HookAtMS))
		}},
		{name: "ended launch with live binding", before: func(f *fixture, _ session, c *HookEvent, _ *AdapterEvidence) {
			if _, err := f.db.Exec(`UPDATE adapter_launch SET ended_hook_at_ms = ? WHERE launch_ref = ?`, c.HookAtMS, c.LaunchRef); err != nil {
				f.t.Fatal(err)
			}
		}},
		{name: "departed binding", before: func(f *fixture, reader session, _ *HookEvent, _ *AdapterEvidence) {
			if _, err := f.db.Exec(`UPDATE binding SET state = 'departed' WHERE principal_id = ?`, f.principalOf(reader)); err != nil {
				f.t.Fatal(err)
			}
		}},
		{name: "different build in Stop", before: func(_ *fixture, _ session, c *HookEvent, e *AdapterEvidence) {
			c.Profile.VendorVersion = "2.0.0"
			e.VendorVersion = c.Profile.VendorVersion
		}},
		{name: "unverified build", before: func(f *fixture, _ session, c *HookEvent, _ *AdapterEvidence) {
			c.Profile.VendorVersion = "unverified"
			if _, err := f.db.Exec(`UPDATE adapter_launch SET profile = ? WHERE launch_ref = ?`, profileJSON(c.Profile), c.LaunchRef); err != nil {
				f.t.Fatal(err)
			}
		}},
		{name: "different manifest evidence", before: func(_ *fixture, _ session, _ *HookEvent, e *AdapterEvidence) { e.ManifestRev = "other" }},
		{name: "different platform evidence", before: func(_ *fixture, _ session, _ *HookEvent, e *AdapterEvidence) { e.Platform = "other" }},
		{name: "different mode evidence", before: func(_ *fixture, _ session, _ *HookEvent, e *AdapterEvidence) { e.Mode = "other" }},
		{name: "candidate evidence", before: func(_ *fixture, _ session, _ *HookEvent, e *AdapterEvidence) { e.Results[0].Level = "candidate" }},
		{name: "unavailable evidence", before: func(_ *fixture, _ session, _ *HookEvent, e *AdapterEvidence) { e.Results[0].Level = "unavailable" }},
		{name: "no evidence", before: func(_ *fixture, _ session, _ *HookEvent, e *AdapterEvidence) { e.Results = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, reader, writer := turnEndFixture(t, "launch")
			f.turnEndAsk(writer, "reader", "unread")
			cmd := stop("launch", "turn-1", f.now.UnixMilli()+1)
			evidence := turnEndEvidence("tested")
			tc.before(f, reader, &cmd, &evidence)
			if len(evidence.Results) != 0 {
				wantOK(t, f.owner(evidence, "evidence"))
			}
			if result := f.hook(reader, cmd); result.TurnEndLine != "" {
				t.Fatalf("ineligible Stop printed %q", result.TurnEndLine)
			}
			if n := f.count(`SELECT COUNT(*) FROM turn_end_notice`); n != 0 {
				t.Fatalf("ineligible Stop wrote %d claims", n)
			}
		})
	}
}

func TestTurnEndNoticeHonorsExactEvidenceOverManifestFallback(t *testing.T) {
	for _, tc := range []struct {
		name, level string
		want        bool
	}{
		{"manifest evidence", "", true},
		{"exact candidate", "candidate", false},
		{"exact unavailable", "unavailable", false},
		{"exact tested", "tested", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, reader, writer := turnEndFixture(t, "launch")
			e := turnEndEvidence("tested")
			e.VendorVersion = "2.0.0"
			wantOK(t, f.owner(e, "manifest-evidence"))
			if tc.level != "" {
				wantOK(t, f.owner(turnEndEvidence(tc.level), "exact-evidence"))
			}
			id := f.turnEndAsk(writer, "reader", "unread")
			var wantLine string
			var wantClaims int
			if tc.want {
				wantLine = fmt.Sprintf("[crosshall] pending: %d", id)
				wantClaims = 1
			}
			got := f.hook(reader, stop("launch", "turn-1", f.now.UnixMilli()+1))
			claims := f.count(`SELECT COUNT(*) FROM turn_end_notice`)
			if got.TurnEndLine != wantLine || claims != wantClaims {
				t.Fatalf("Stop printed %q with %d claims, want %q with %d claims", got.TurnEndLine, claims, wantLine, wantClaims)
			}
		})
	}
}

func TestTurnEndNoticeCapsOldestPerRecipientAcrossLaunches(t *testing.T) {
	f, reader, writer := turnEndFixture(t, "launch")
	wantOK(t, f.owner(turnEndEvidence("tested"), "evidence"))
	other := f.newSession("claude")
	wantOK(t, f.join(other, Join{Channel: "checkout", Alias: "other"}, "join-other"))
	f.hook(other, start("other-launch", "startup", f.now.UnixMilli()))
	f.hook(other, prompt("other-launch", "other-turn", f.now.UnixMilli()+1))
	if _, err := f.db.Exec(`UPDATE binding SET dnd = 1 WHERE principal_id = ?`, f.principalOf(other)); err != nil {
		t.Fatal(err)
	}

	var ids []string
	for i := range OfferCap + 1 {
		r := f.ask(writer, Ask{To: []string{"reader", "other"}, New: true, Body: "bounded group"}, fmt.Sprintf("ask-%d", i))
		wantOK(t, r)
		ids = append(ids, strconv.FormatInt(decode[AskResult](t, r).ID, 10))
	}
	want := "[crosshall] pending: " + strings.Join(ids[:OfferCap], " ")
	if r := f.hook(reader, stop("launch", "turn-1", f.now.UnixMilli()+1)); r.TurnEndLine != want {
		t.Fatalf("first recipient line = %q, want %q", r.TurnEndLine, want)
	}
	if r := f.hook(other, stop("other-launch", "other-turn", f.now.UnixMilli()+1)); r.TurnEndLine != want {
		t.Fatalf("second recipient line = %q, want %q", r.TurnEndLine, want)
	}
	f.hook(reader, start("fresh-launch", "startup", f.now.UnixMilli()+1))
	f.hook(reader, prompt("fresh-launch", "turn-fresh", f.now.UnixMilli()+1))
	want = "[crosshall] pending: " + ids[OfferCap]
	if r := f.hook(reader, stop("fresh-launch", "turn-fresh", f.now.UnixMilli()+1)); r.TurnEndLine != want {
		t.Fatalf("new launch line = %q, want %q", r.TurnEndLine, want)
	}
	if n := f.count(`SELECT COUNT(*) FROM announcement`); n != 2*OfferCap+1 {
		t.Fatalf("claimed %d recipient rows, want %d", n, 2*OfferCap+1)
	}
}

func TestTurnEndNoticeAllowsAttestedSessionScopedLaunch(t *testing.T) {
	f, writer := adapterChannel(t)
	f.policy("codex", true, 1)
	reader := f.newSession("codex")
	wantOK(t, f.join(reader, Join{Channel: "checkout", Alias: "reader"}, "join-reader"))
	p := testProfile
	p.Adapter, p.VendorVersion, p.Mode, p.ManifestRev = "codex", "1.2.3", "exec", "codex-1"
	e := turnEndEvidence("tested")
	e.Vendor, e.VendorVersion, e.Mode, e.ManifestRev = p.Adapter, p.VendorVersion, p.Mode, p.ManifestRev
	wantOK(t, f.owner(e, "codex-evidence"))

	enroll := start(SessionLaunch, "startup", f.now.UnixMilli())
	enroll.Profile = p
	f.hook(reader, enroll)
	open := prompt(SessionLaunch, "turn", f.now.UnixMilli()+1)
	open.Profile = p
	f.hook(reader, open)

	id := f.turnEndAsk(writer, "reader", "unread")
	finish := stop(SessionLaunch, "turn", f.now.UnixMilli()+1)
	finish.Profile = p
	if r := f.hook(reader, finish); r.TurnEndLine != fmt.Sprintf("[crosshall] pending: %d", id) {
		t.Fatalf("attested session-scoped launch returned %+v", r)
	}
}

// An ask the turn-end notice announced is not offered again by the next
// prompt; what the session was never told about still is.
func TestPromptDoesNotRepeatATurnEndNotice(t *testing.T) {
	f, reader, writer := turnEndFixture(t, "launch")
	wantOK(t, f.owner(turnEndEvidence("tested"), "evidence"))
	announced := f.turnEndAsk(writer, "reader", "during the turn")
	at := f.now.UnixMilli()
	if got := f.hook(reader, stop("launch", "turn-1", at+2)); got.TurnEndLine != fmt.Sprintf("[crosshall] pending: %d", announced) {
		t.Fatalf("turn-end result = %+v", got)
	}
	if got := f.hook(reader, prompt("launch", "turn-2", at+3)); got.PendingLine != "" {
		t.Fatalf("the next prompt repeated the notice: %+v", got)
	}
	later := f.noteTo(writer, "reader", 1)
	if got := f.hook(reader, prompt("launch", "turn-3", at+4)); got.PendingLine != pendingLine(later...) {
		t.Fatalf("a new note was not offered: %+v", got)
	}
}

// #198: the owner's turn-end switch stops the continuation for one agent
// kind without a new policy epoch, and default lets the evidence decide.
func TestTurnEndNoticeFollowsTheOwnersSwitch(t *testing.T) {
	f, reader, writer := turnEndFixture(t, "launch")
	wantOK(t, f.owner(turnEndEvidence("tested"), "evidence"))
	set := func(value, key string) store.AdapterPolicy {
		t.Helper()
		r := f.owner(AdapterSet{wire.AdapterPolicyArgs{Adapter: "claude", TurnEnd: &value}}, key)
		wantOK(t, r)
		return decode[store.AdapterPolicy](t, r)
	}
	before := set("default", "turn-end-default-first")
	off := set("off", "turn-end-off")
	if off.TurnEnd != "off" || off.Epoch != before.Epoch || !off.Enabled {
		t.Fatalf("switch off gave %+v after %+v", off, before)
	}
	ask := f.turnEndAsk(writer, "reader", "while-off")
	if r := f.hook(reader, stop("launch", "turn-1", f.now.UnixMilli()+1)); r.TurnEndLine != "" {
		t.Fatalf("a Stop with the switch off printed %q", r.TurnEndLine)
	}
	if again := set("default", "turn-end-default"); again.TurnEnd != "" {
		t.Fatalf("default stored %q", again.TurnEnd)
	}
	f.hook(reader, prompt("launch", "turn-2", f.now.UnixMilli()+1))
	if r := f.hook(reader, stop("launch", "turn-2", f.now.UnixMilli()+1)); r.TurnEndLine != fmt.Sprintf("[crosshall] pending: %d", ask) {
		t.Fatalf("a Stop after default = %+v, want ask %d", r, ask)
	}
	if r := f.owner(AdapterSet{wire.AdapterPolicyArgs{Adapter: "agy", TurnEnd: &[]string{"on"}[0]}}, "agy2-on"); r.Status != core.ExitOK {
		t.Fatalf("Antigravity agy-2 refused --turn-end on: %+v", r)
	}
	withSessionStartOnlyAgy(t)
	if r := f.owner(AdapterSet{wire.AdapterPolicyArgs{Adapter: "agy", TurnEnd: &[]string{"on"}[0]}}, "agy-on"); r.Status == core.ExitOK {
		t.Fatal("Antigravity agy-1 accepted --turn-end on")
	}
	// The way back that doctor names after an Antigravity off is accepted.
	for _, value := range []string{"off", "default"} {
		if r := f.owner(AdapterSet{wire.AdapterPolicyArgs{Adapter: "agy", TurnEnd: &value}}, "agy-"+value); r.Status != core.ExitOK {
			t.Fatalf("Antigravity --turn-end %s: %+v", value, r)
		}
	}
}

// #261: a Stop's hook skips its wait only while TurnEndAskWaiting is false,
// so it must hold for every ask a turn-end notice could still announce.
func TestTurnEndAskWaitingFollowsWhatANoticeCouldAnnounce(t *testing.T) {
	f, reader, writer := turnEndFixture(t, "launch")
	wantOK(t, f.owner(turnEndEvidence("tested"), "evidence"))
	pid := f.principalOf(reader)
	waiting := func(want bool, when string) {
		t.Helper()
		if got, err := TurnEndAskWaiting(t.Context(), f.db, pid); err != nil || got != want {
			t.Fatalf("%s: waiting = %t (%v), want %t", when, got, err, want)
		}
	}
	waiting(false, "no message")
	f.turnEndAsk(reader, "writer", "outgoing")
	waiting(false, "the member's own ask")
	f.noteTo(writer, "reader", 1)
	waiting(false, "a note")
	read := f.turnEndAsk(writer, "reader", "to read")
	waiting(true, "an unread ask")
	wantOK(t, f.run(reader, Read{IDs: []int64{read}}, "read"))
	waiting(false, "the ask read")
	cancelled := f.turnEndAsk(writer, "reader", "to cancel")
	waiting(true, "an ask before its cancel")
	wantOK(t, f.owner(Cancel{Channel: "checkout", ID: cancelled}, "cancel"))
	waiting(false, "the ask cancelled")
	announced := f.turnEndAsk(writer, "reader", "to announce")
	waiting(true, "an ask before its notice")
	if r := f.hook(reader, stop("launch", "turn-1", f.now.UnixMilli()+1)); r.TurnEndLine != fmt.Sprintf("[crosshall] pending: %d", announced) {
		t.Fatalf("turn-end result = %+v", r)
	}
	waiting(false, "the ask announced")
	f.turnEndAsk(writer, "reader", "before leaving")
	waiting(true, "an ask before the member left")
	if _, err := f.db.Exec(`UPDATE binding SET state = 'departed' WHERE principal_id = ?`, pid); err != nil {
		t.Fatal(err)
	}
	waiting(false, "the member left")
}

// #261 review: a notice that was not submitted gives its ask back to the
// turn-end line. The member is marked dirty, so the record a Stop reads is
// raised again, also after a hook lowered it while the attempt was open.
func TestANoticeNotSubmittedMarksItsMemberForTheTurnEndRecord(t *testing.T) {
	for _, how := range []string{"transport", "retired queue"} {
		t.Run(how, func(t *testing.T) {
			p := newPublishFixture(t)
			p.proveRoute()
			wantOK(t, p.run(p.reader, Read{IDs: []int64{p.beforeID}}, "read-before-route"))
			pid := p.principalOf(p.reader)
			p.turnEndAsk(p.writer, "reader", "during the turn")
			id := p.reserve()
			if id == 0 {
				t.Fatal("no notice reserved")
			}
			waiting := func() bool {
				t.Helper()
				got, err := TurnEndAskWaiting(t.Context(), p.db, pid)
				if err != nil {
					t.Fatal(err)
				}
				return got
			}
			if waiting() {
				t.Fatal("the reserved notice did not take the ask")
			}
			p.mutate(`DELETE FROM adapter_dirty`) // a hook of the member applied the stamps
			if how == "transport" {
				p.begin(id)
				p.resolve(id, PublishNotSubmitted)
			} else {
				p.update(func(ctx context.Context, tx store.Tx) error { return RetirePublishQueue(ctx, tx, p.queue, p.now) })
			}
			p.state(id, string(PublishNotSubmitted))
			if !waiting() {
				t.Fatal("the ask did not come back to the turn-end line")
			}
			if n := p.count(`SELECT COUNT(*) FROM adapter_dirty WHERE principal_id = ? AND notice = 1`, pid); n != 1 {
				t.Fatalf("the member is not marked dirty: %d rows", n)
			}
		})
	}
}
