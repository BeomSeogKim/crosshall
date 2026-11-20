package domain

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
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
	f.roots(writer, 20)
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
	f.roots(reader, 1)
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
	f.roots(writer, 1)
	id := f.turnEndAsk(writer, "reader", "unread")
	finish := stop(SessionLaunch, "turn", f.now.UnixMilli()+1)
	finish.Profile = p
	if r := f.hook(reader, finish); r.TurnEndLine != fmt.Sprintf("[crosshall] pending: %d", id) {
		t.Fatalf("attested session-scoped launch returned %+v", r)
	}
}
