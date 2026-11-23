package domain

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

func TestEvaluateGuardRoundDecayAndBurstBoundaries(t *testing.T) {
	const now = 1_800_000_000_000
	for _, tt := range []struct {
		name              string
		position          int64
		requests, answers int64
		wantRound         int64
		reason            string
	}{
		{"fresh", 0, 1, 1, 1, ""},
		{"last allowed round", now + 7*roundDurationMS, 1, 1, 8, ""},
		{"next dependent round", now + 8*roundDurationMS, 1, 1, 9, "round_limit"},
		{"one millisecond before a round returns", now + 7*roundDurationMS + 1, 1, 1, 9, "round_limit"},
		{"at the burst ceilings", 0, 30, 60, 1, ""},
		{"request burst", 0, 31, 31, 1, "too_many_requests"},
		{"answer burst", 0, 6, 61, 1, "too_many_requests"},
		{"burst before round", now + 8*roundDurationMS, 31, 61, 9, "too_many_requests"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := evaluateGuard(now, tt.position, 8, tt.requests, tt.answers, 1)
			if got.Round != tt.wantRound || got.Reason != tt.reason {
				t.Fatalf("decision %+v, want round %d reason %q", got, tt.wantRound, tt.reason)
			}
		})
	}
	if got := evaluateGuard(now, 0, 8, 1, 61, 61); got.Reason != "audience_too_large" {
		t.Fatalf("oversized audience: %+v", got)
	}
	minute := int64(time.Minute / time.Millisecond)
	stamp := evaluateGuard(now+5*minute, now+60*minute, 8, 1, 1, 1)
	if stamp.LevelMS != now+70*minute || roundAt(stamp.LevelMS, now+10*minute-1) != 7 ||
		roundAt(stamp.LevelMS, now+10*minute) != 6 || roundAt(stamp.LevelMS, now+20*minute) != 5 {
		t.Fatalf("the first decay tick occurs at the next level boundary: %+v", stamp)
	}
	var position int64
	for i := int64(0); i < 10; i++ {
		got := evaluateGuard(now+i*roundDurationMS, position, 8, 1, 1, 1)
		if got.Round != 1 || got.Reason != "" {
			t.Fatalf("slow chain step %d: %+v", i, got)
		}
		position = got.LevelMS
	}
}

func TestGuardRecordsRoundNineAfterLegacyAcceptanceAndOnlyOnce(t *testing.T) {
	f, a, b, _ := channel3(t)
	f.roots(a, 1)
	sent := f.guardAsk(a, Ask{To: []string{"tax"}, Body: "first"}, "first")
	executeGuardFixture(t, f, `UPDATE exchange SET granted = 32 WHERE id = ?`, sent.Exchange)
	var lastAsk Ask
	var lastSender session
	for round := 2; round <= 9; round++ {
		lastSender = b
		recipient := "form"
		if round%2 == 1 {
			lastSender, recipient = a, "tax"
		}
		wantOK(t, f.exec(f.caller(lastSender), Request{Command: Read{IDs: []int64{sent.ID}}}))
		lastAsk = Ask{Re: sent.ID, To: []string{recipient}, Body: "continue"}
		sent = f.guardAsk(lastSender, lastAsk, fmt.Sprint(round))
		if got := f.messageLevel(sent.ID); got != ms(f.now)+int64(round)*roundDurationMS {
			t.Fatalf("level at round %d = %d", round, got)
		}
		if got := f.anyGuardLevel(lastSender); got != ms(f.now)+int64(round-1)*roundDurationMS {
			t.Fatalf("sending advanced the sender at round %d: %d", round, got)
		}
	}
	before := f.state()
	replayed := f.guardAsk(lastSender, lastAsk, "9")
	if replayed.ID != sent.ID || f.state() != before {
		t.Fatalf("replay changed the ledger: %+v", replayed)
	}
	got := f.guardEvent(sent.ID)
	if got.Round != 9 || got.Reason != "round_limit" || got.Path != sent.Exchange ||
		f.count(`SELECT COUNT(*) FROM event WHERE kind = ?`, guardWouldRefuse) != 1 {
		t.Fatalf("round audit: %+v", got)
	}
	for _, result := range []core.Result{
		f.exec(f.caller(lastSender), Request{Command: Status{}}),
		f.owner(OwnerStatus{Channel: "checkout"}, ""),
	} {
		wantOK(t, result)
		if strings.Contains(string(result.Data), "round_limit") || strings.Contains(string(result.Data), "level_ms") {
			t.Fatalf("new guard data appeared in status: %s", result.Data)
		}
	}
}

func TestGuardKeepsMixedInboxPathsAndNewFallback(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprint("reverse=", reverse), func(t *testing.T) {
			f, a, b, c := channel3(t)
			f.roots(a, 2)
			f.roots(c, 1)
			deep := f.guardAsk(a, Ask{To: []string{"tax"}, Body: "deep"}, "deep")
			deepLevel := ms(f.now) + 8*roundDurationMS
			executeGuardFixture(t, f, `UPDATE message SET level_ms = ? WHERE id = ?`, deepLevel, deep.ID)

			result := f.reply(b, Reply{To: deep.ID, Body: "answer without reading"}, "reply")
			wantOK(t, result)
			reply := decode[ReplyResult](t, result)
			if !reply.OriginalUnread || f.messageLevel(reply.ID) != deepLevel || f.anyGuardLevel(b) != deepLevel ||
				f.count(`SELECT COUNT(*) FROM receipt WHERE message_id = ? AND stage = 'picked_up'`, deep.ID) != 0 {
				t.Fatalf("direct reply changed receipt semantics: %+v", reply)
			}
			fresh := f.guardAsk(c, Ask{To: []string{"form"}, Body: "independent"}, "fresh")
			ids := []int64{deep.ID, fresh.ID}
			if reverse {
				ids[0], ids[1] = ids[1], ids[0]
			}
			read := Read{IDs: ids}
			for _, key := range []string{"read", "read", "new-read"} {
				wantOK(t, f.exec(f.caller(a), Request{OpKey: key, Command: read}))
			}
			if f.anyGuardLevel(a) != deepLevel ||
				f.count(`SELECT COUNT(*) FROM receipt WHERE message_id = ? AND stage = 'picked_up'`, reply.ID) != 1 {
				t.Fatal("nested reply did not fold once")
			}
			delegated := f.guardAsk(a, Ask{To: []string{"tax"}, Body: "delegate fresh work"}, "delegate")
			if delegated.Exchange != fresh.Exchange || f.messageLevel(delegated.ID) != ms(f.now)+2*roundDurationMS {
				t.Fatalf("fresh delegation used the unrelated deeper position: %+v", delegated)
			}
			executeGuardFixture(t, f, `UPDATE exchange SET granted = spent WHERE id = ?`, fresh.Exchange)
			handoff := f.guardAsk(a, Ask{New: true, Re: fresh.ID, To: []string{"tax"}, Body: "open a handoff"}, "handoff")
			if !handoff.NewExchange || handoff.ParentRequestID != fresh.ID {
				t.Fatalf("legacy handoff changed: %+v", handoff)
			}
			if got := f.guardEvent(handoff.ID); got.Path != 0 || got.Round != 9 || got.Reason != "round_limit" {
				t.Fatalf("new handoff should use any-path: %+v", got)
			}
		})
	}
}

func TestGuardCountsUnreadAndExplicitParents(t *testing.T) {
	for _, name := range []string{"inferred unread", "explicit unread", "explicit without obligation"} {
		t.Run(name, func(t *testing.T) {
			f, a, b, _ := channel3(t)
			f.roots(a, 1)
			parent := f.guardAsk(a, Ask{To: []string{"tax"}, Body: "deep request"}, "parent")
			executeGuardFixture(t, f, `UPDATE message SET level_ms = ? WHERE id = ?`, ms(f.now)+8*roundDurationMS, parent.ID)
			sender, target := b, "form"
			var reference int64
			if name != "inferred unread" {
				reference = parent.ID
			}
			if name == "explicit without obligation" {
				sender, target = a, "tax"
			}
			sent := f.guardAsk(sender, Ask{To: []string{target}, Re: reference, Body: "delegate"}, "delegate")
			got := f.guardEvent(sent.ID)
			if got.Round != 9 || got.Reason != "round_limit" || got.Path != parent.Exchange {
				t.Fatalf("parent was ignored: %+v", got)
			}
			if f.anyGuardLevel(sender) != 0 ||
				f.count(`SELECT COUNT(*) FROM receipt WHERE message_id = ? AND stage = 'picked_up'`, parent.ID) != 0 {
				t.Fatal("sending changed the position or receipt of an unread request")
			}
		})
	}
}

func TestGuardLinkedHandoffUsesTheHigherParentEvaluation(t *testing.T) {
	f, a, b, _ := channel3(t)
	f.roots(a, 1)
	f.roots(b, 1)
	parent := f.guardAsk(a, Ask{To: []string{"tax"}, Body: "deep request"}, "parent")
	executeGuardFixture(t, f, `UPDATE message SET level_ms = ? WHERE id = ?`, ms(f.now)+8*roundDurationMS, parent.ID)
	wantOK(t, f.exec(f.caller(b), Request{Command: Read{IDs: []int64{parent.ID}}}))
	executeGuardFixture(t, f, `UPDATE principal SET any_level_ms = 0, reset_after_message_id = ? WHERE id = ?`,
		parent.ID, *f.caller(b).PrincipalID)
	executeGuardFixture(t, f, `UPDATE exchange SET granted = spent WHERE id = ?`, parent.Exchange)
	sent := f.guardAsk(b, Ask{To: []string{"form"}, New: true, Re: parent.ID, Body: "handoff"}, "handoff")
	got := f.guardEvent(sent.ID)
	if !sent.NewExchange || sent.ParentRequestID != parent.ID || got.Path != parent.Exchange || got.Round != 9 {
		t.Fatalf("handoff shed its parent round: send=%+v audit=%+v", sent, got)
	}
}

func TestGuardFencesDelayedReplyAtItsOriginalRequest(t *testing.T) {
	f, a, b, _ := channel3(t)
	f.roots(a, 1)
	parent := f.guardAsk(a, Ask{To: []string{"tax"}, Body: "before reset"}, "parent")
	level := ms(f.now) + 8*roundDurationMS
	executeGuardFixture(t, f, `UPDATE message SET level_ms = ? WHERE id = ?`, level, parent.ID)
	executeGuardFixture(t, f, `UPDATE exchange SET reset_after_message_id = ? WHERE id = ?`, parent.ID, parent.Exchange)
	executeGuardFixture(t, f, `UPDATE principal SET reset_after_message_id = ? WHERE id = ?`, parent.ID, *f.caller(a).PrincipalID)
	r := f.reply(b, Reply{To: parent.ID, Body: "after reset"}, "answer")
	wantOK(t, r)
	reply := decode[ReplyResult](t, r)
	if reply.ID <= parent.ID || f.messageLevel(reply.ID) != level {
		t.Fatalf("a reply must carry the original level: %+v", reply)
	}
	wantOK(t, f.exec(f.caller(a), Request{Command: Read{IDs: []int64{reply.ID}}}))
	if f.anyGuardLevel(a) != 0 || f.count(`SELECT COUNT(*) FROM thread_position`) != 0 {
		t.Fatal("a reply published after the reset revived the old round")
	}
	followup := f.guardAsk(a, Ask{To: []string{"tax"}, Re: reply.ID, Body: "after reset"}, "followup")
	if got := f.messageLevel(followup.ID); got != ms(f.now)+roundDurationMS {
		t.Fatalf("an explicit reference to the old reply advanced the new round: %d", got)
	}
}

func TestGuardBurstIncludesSettledAnswersAndExpiresAtTenMinutes(t *testing.T) {
	f, a, b, _ := channel3(t)
	f.roots(a, 1)
	send := Ask{All: true, Each: true, Body: "independent work"}
	first := f.guardAsk(a, send, "burst-1")
	executeGuardFixture(t, f, `UPDATE exchange SET granted = 64 WHERE id = ?`, first.Exchange)
	wantOK(t, f.reply(b, Reply{To: first.ID, Body: "done"}, "settled"))
	checkLevel := func(got AskResult) {
		t.Helper()
		if f.messageLevel(got.ID) != ms(f.now)+roundDurationMS {
			t.Fatalf("independent request %d advanced its round", got.ID)
		}
	}
	var workers sync.WaitGroup
	completed := make(chan core.Result, 12)
	for i := 2; i <= 13; i++ {
		workers.Go(func() { completed <- f.ask(a, send, fmt.Sprintf("burst-%d", i)) })
	}
	workers.Wait()
	close(completed)
	if len(completed) != 12 {
		t.Fatalf("completed concurrent requests = %d, want 12", len(completed))
	}
	for result := range completed {
		wantOK(t, result)
		checkLevel(decode[AskResult](t, result))
	}
	for i := 14; i <= 30; i++ {
		checkLevel(f.guardAsk(a, send, fmt.Sprintf("burst-%d", i)))
	}
	if f.count(`SELECT COUNT(*) FROM event WHERE kind = ?`, guardWouldRefuse) != 0 {
		t.Fatal("guard recorded a refusal at or below the burst ceilings")
	}
	over := f.guardAsk(a, send, "over")
	if got := f.guardEvent(over.ID); got.Requests != 31 || got.OwedAnswers != 62 || got.Reason != "too_many_requests" {
		t.Fatalf("burst audit: %+v", got)
	}
	f.now = f.now.Add(10 * time.Minute)
	f.guardAsk(a, send, "aged")
	if f.count(`SELECT COUNT(*) FROM event WHERE kind = ?`, guardWouldRefuse) != 1 {
		t.Fatal("the burst window retained sends exactly ten minutes old")
	}
}

func TestGuardPreservesLegacyBudgetRefusal(t *testing.T) {
	f, a, _, _ := channel3(t)
	f.roots(a, 1)
	cmd := Ask{To: []string{"tax"}, Body: "review"}
	for i := 1; i <= 8; i++ {
		f.guardAsk(a, cmd, fmt.Sprint(i))
	}
	executeGuardFixture(t, f, `UPDATE principal SET any_level_ms = ? WHERE id = ?`, ms(f.now)+8*roundDurationMS, *f.caller(a).PrincipalID)
	before := f.count(`SELECT COUNT(*) FROM message`)
	wantCode(t, f.ask(a, cmd, "exhausted"), core.ExitRefused, core.CodeBudgetExhausted)
	if f.count(`SELECT COUNT(*) FROM message`) != before ||
		f.count(`SELECT COUNT(*) FROM event WHERE kind = ?`, guardWouldRefuse) != 0 {
		t.Fatal("old refused ask wrote a message or a shadow event")
	}
}

func TestGuardPreservesOldReplayAndUnreadReply(t *testing.T) {
	f, a, b, _ := channel3(t)
	f.roots(a, 1)
	cmd := Ask{To: []string{"tax"}, Body: "before upgrade"}
	first := f.guardAsk(a, cmd, "saved")
	executeGuardFixture(t, f, `UPDATE message SET level_ms = 0 WHERE id = ?`, first.ID)
	executeGuardFixture(t, f, `UPDATE principal SET any_level_ms = ? WHERE id = ?`, ms(f.now)+8*roundDurationMS, *f.caller(a).PrincipalID)
	again := f.guardAsk(a, cmd, "saved")
	if again.ID != first.ID || f.messageLevel(first.ID) != 0 || f.count(`SELECT COUNT(*) FROM event WHERE kind = ?`, guardWouldRefuse) != 0 {
		t.Fatalf("old operation key changed: %+v", again)
	}
	r := f.reply(b, Reply{To: first.ID, Body: "answer after upgrade"}, "old-unread")
	wantOK(t, r)
	reply := decode[ReplyResult](t, r)
	if !reply.OriginalUnread || f.messageLevel(reply.ID) != 0 || f.anyGuardLevel(b) != 0 {
		t.Fatalf("old unread request invented a position: %+v", reply)
	}
}

func TestGuardNotesUseAnyPathAndRespectPickupWatermark(t *testing.T) {
	f, a, b, c := channel3(t)
	level := ms(f.now) + 5*roundDurationMS
	executeGuardFixture(t, f, `UPDATE principal SET any_level_ms = ? WHERE id = ?`, level, *f.caller(a).PrincipalID)
	r := f.exec(f.caller(a), Request{OpKey: "note", Command: Note{All: true, Body: "information"}})
	wantOK(t, r)
	id := decode[NoteResult](t, r).ID
	executeGuardFixture(t, f, `UPDATE principal SET reset_after_message_id = ? WHERE id = ?`, id, *f.caller(c).PrincipalID)
	for _, recipient := range []session{b, c} {
		wantOK(t, f.exec(f.caller(recipient), Request{Command: Read{IDs: []int64{id}}}))
	}
	if f.messageLevel(id) != level || f.anyGuardLevel(b) != level || f.anyGuardLevel(c) != 0 ||
		f.count(`SELECT COUNT(*) FROM thread_position`) != 0 {
		t.Fatal("note level, any-path fold or reset watermark was lost")
	}
}

func TestGuardPathWatermarkLeavesAnyPathIndependent(t *testing.T) {
	f, a, b, _ := channel3(t)
	f.roots(a, 1)
	sent := f.guardAsk(a, Ask{To: []string{"tax"}, Body: "before reset"}, "before")
	executeGuardFixture(t, f, `UPDATE exchange SET reset_after_message_id = ? WHERE id = ?`, sent.ID, sent.Exchange)
	wantOK(t, f.exec(f.caller(b), Request{Command: Read{IDs: []int64{sent.ID}}}))
	if f.anyGuardLevel(b) != f.messageLevel(sent.ID) || f.count(`SELECT COUNT(*) FROM thread_position`) != 0 {
		t.Fatal("path reset leaked into the any-path position")
	}
}

func (f *fixture) guardAsk(s session, cmd Ask, key string) AskResult {
	f.t.Helper()
	result := f.ask(s, cmd, key)
	wantOK(f.t, result)
	return decode[AskResult](f.t, result)
}

func (f *fixture) guardEvent(id int64) guardDecision {
	f.t.Helper()
	var raw []byte
	if err := f.db.QueryRow(`SELECT detail FROM event WHERE kind = ? AND subject = ?`, guardWouldRefuse, fmt.Sprintf("message:%d", id)).Scan(&raw); err != nil {
		f.t.Fatal(err)
	}
	var out guardDecision
	if err := json.Unmarshal(raw, &out); err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *fixture) messageLevel(id int64) int64 {
	f.t.Helper()
	var level int64
	if err := f.db.QueryRow(`SELECT level_ms FROM message WHERE id = ?`, id).Scan(&level); err != nil {
		f.t.Fatal(err)
	}
	return level
}

func (f *fixture) anyGuardLevel(s session) int64 {
	f.t.Helper()
	var level int64
	if err := f.db.QueryRow(`SELECT any_level_ms FROM principal WHERE id = ?`, *f.caller(s).PrincipalID).Scan(&level); err != nil {
		f.t.Fatal(err)
	}
	return level
}

func executeGuardFixture(t *testing.T, f *fixture, query string, args ...any) {
	t.Helper()
	if _, err := f.db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}
