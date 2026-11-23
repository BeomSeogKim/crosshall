package domain

import (
	"encoding/json"
	"fmt"
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

func TestGuardKeepsMixedInboxPathsAndNewFallback(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprint("reverse=", reverse), func(t *testing.T) {
			f, a, b, c := channel3(t)
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
			refused := f.ask(a, Ask{New: true, Re: fresh.ID, To: []string{"tax"}, Body: "open a handoff"}, "handoff")
			wantCode(t, refused, core.ExitRefused, core.CodeRoundLimit)
			got := f.stopEvent(fmt.Sprintf("principal:%d", *f.caller(a).PrincipalID))
			if got.ThreadRootID != 0 || got.Round != 9 {
				t.Fatalf("new handoff should use member-wide evaluation: %+v", got)
			}
		})
	}
}

func TestGuardCountsUnreadAndExplicitParents(t *testing.T) {
	for _, name := range []string{"inferred unread", "explicit unread", "explicit without obligation"} {
		t.Run(name, func(t *testing.T) {
			f, a, b, _ := channel3(t)
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
			result := f.ask(sender, Ask{To: []string{target}, Re: reference, Body: "delegate"}, "delegate")
			wantCode(t, result, core.ExitRefused, core.CodeRoundLimit)
			got := f.stopEvent(fmt.Sprintf("exchange:%d", parent.Exchange))
			if got.Round != 9 || got.ThreadRootID != parent.ID {
				t.Fatalf("parent was ignored: %+v", got)
			}
			if f.anyGuardLevel(sender) != 0 ||
				f.count(`SELECT COUNT(*) FROM receipt WHERE message_id = ? AND stage = 'picked_up'`, parent.ID) != 0 {
				t.Fatal("sending changed the position or receipt of an unread request")
			}
		})
	}
}

func TestGuardSeparatesIndependentRootsInOneExchange(t *testing.T) {
	for _, explicit := range []bool{true, false} {
		t.Run(fmt.Sprint("explicit=", explicit), func(t *testing.T) {
			f, a, b, _ := channel3(t)
			first := f.guardAsk(a, Ask{To: []string{"tax"}, Body: "first task"}, "first")
			second := f.guardAsk(a, Ask{To: []string{"tax"}, Body: "independent task"}, "second")
			if first.Exchange == second.Exchange {
				t.Fatal("new roots must open distinct exchanges")
			}
			executeGuardFixture(t, f, `UPDATE message SET exchange_id = ? WHERE id = ?`, first.Exchange, second.ID)
			deepLevel := ms(f.now) + 8*roundDurationMS
			executeGuardFixture(t, f, `UPDATE message SET level_ms = ? WHERE id = ?`, deepLevel, first.ID)
			result := f.reply(b, Reply{To: first.ID, Body: "answer the deep task"}, "reply")
			wantOK(t, result)
			reply := decode[ReplyResult](t, result)
			wantOK(t, f.exec(f.caller(a), Request{Command: Read{IDs: []int64{reply.ID}}}))
			sender := b
			cmd := Ask{To: []string{"form"}, Body: "follow the independent task"}
			if explicit {
				sender = a
				cmd.Re, cmd.To = second.ID, []string{"tax"}
			}
			if f.anyGuardLevel(sender) != deepLevel {
				t.Fatal("the deep task did not reach the sender's any-path position")
			}
			sent := f.guardAsk(sender, cmd, "followup")
			if got := roundAt(f.messageLevel(sent.ID), ms(f.now)); got != 2 {
				t.Fatalf("independent task reached round %d, want 2", got)
			}
			if f.count(`SELECT COUNT(*) FROM event WHERE kind = ?`, guardStopped) != 0 {
				t.Fatal("independent task produced a refusal audit")
			}
		})
	}
}

func TestGuardLinkedHandoffUsesTheHigherParentEvaluation(t *testing.T) {
	f, a, b, _ := channel3(t)
	parent := f.guardAsk(a, Ask{To: []string{"tax"}, Body: "deep request"}, "parent")
	executeGuardFixture(t, f, `UPDATE message SET level_ms = ? WHERE id = ?`, ms(f.now)+7*roundDurationMS, parent.ID)
	f.grantMember("tax", 8)
	linked := f.guardAsk(b, Ask{To: []string{"form"}, New: true, Re: parent.ID, Body: "handoff without pickup"}, "handoff")
	if !linked.NewExchange || linked.Exchange == parent.Exchange || linked.ParentRequestID != parent.ID || linked.Round != 8 {
		t.Fatalf("linked handoff shed rounds or the parent link: %+v", linked)
	}
	if f.count(`SELECT COUNT(*) FROM message WHERE id = ? AND thread_root_id IS NULL`, linked.ID) != 1 {
		t.Fatal("linked handoff must start its own thread")
	}
	follow := f.guardAsk(b, Ask{To: []string{"form"}, Re: parent.ID, Body: "plain follow-up"}, "follow")
	if follow.Round != linked.Round || follow.Exchange != parent.Exchange {
		t.Fatalf("continuation %+v and handoff %+v disagree on rounds", follow, linked)
	}
	r := f.reply(a, Reply{To: linked.ID, Body: "answer"}, "reply")
	wantOK(t, r)
	reply := decode[ReplyResult](t, r)
	f.grantThread(linked.Exchange, 8)
	f.grantMember("tax", 8)
	viaReply := f.guardAsk(b, Ask{To: []string{"form"}, New: true, Re: reply.ID, Body: "linked to reply"}, "reply-link")
	if viaReply.Round != 1 || viaReply.ParentRequestID != linked.ID || viaReply.Exchange == linked.Exchange {
		t.Fatalf("link to a reply must name its request: %+v", viaReply)
	}
}

func TestGuardFencesDelayedReplyAtItsOriginalRequest(t *testing.T) {
	f, a, b, _ := channel3(t)
	parent := f.guardAsk(a, Ask{To: []string{"tax"}, Body: "before reset"}, "parent")
	level := ms(f.now) + 8*roundDurationMS
	executeGuardFixture(t, f, `UPDATE message SET level_ms = ? WHERE id = ?`, level, parent.ID)
	f.grantThread(parent.Exchange, 8)
	f.grantMember("form", 8)
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

func TestGuardLateReplyHonorsOnlyItsThreadFence(t *testing.T) {
	f, a, b, _ := channel3(t)
	root := f.guardAsk(a, Ask{To: []string{"tax"}, Body: "root"}, "root")
	parent := f.guardAsk(a, Ask{To: []string{"tax"}, Re: root.ID, Body: "continuation"}, "parent")
	level := ms(f.now) + 8*roundDurationMS
	executeGuardFixture(t, f, `UPDATE message SET level_ms = ? WHERE id = ?`, level, parent.ID)
	f.grantThread(root.Exchange, 8)
	result := f.reply(b, Reply{To: parent.ID, Body: "late reply"}, "answer")
	wantOK(t, result)
	reply := decode[ReplyResult](t, result)
	wantOK(t, f.exec(f.caller(a), Request{Command: Read{IDs: []int64{reply.ID}}}))
	if f.anyGuardLevel(a) != level || f.anyGuardLevel(b) != level || f.count(`SELECT COUNT(*) FROM thread_position`) != 0 {
		t.Fatal("thread fence must exclude the reply from the thread and keep its level on the any-path position")
	}
	linked := f.guardAsk(a, Ask{To: []string{"tax"}, Re: reply.ID, Body: "continue after fence"}, "linked")
	if got := roundAt(f.messageLevel(linked.ID), ms(f.now)); got != 1 {
		t.Fatalf("linked follow-up after the thread fence reached round %d, want 1", got)
	}
	denied := f.ask(a, Ask{To: []string{"tax"}, Body: "new task"}, "new")
	wantCode(t, denied, core.ExitRefused, core.CodeRoundLimit)
	if got := f.stopEvent(fmt.Sprintf("principal:%d", *f.caller(a).PrincipalID)); got.ThreadRootID != 0 || got.Round != 9 {
		t.Fatalf("parentless request should still see the late reply's level: %+v", got)
	}
	result = f.reply(b, Reply{To: linked.ID, Body: "reply to a post-fence request"}, "after-fence")
	wantOK(t, result)
	if f.count(`SELECT COUNT(*) FROM thread_position WHERE thread_root_id = ? AND level_ms = ?`, root.ID, f.messageLevel(linked.ID)) != 1 {
		t.Fatal("a reply to a post-fence request must fold on the original thread")
	}
}

func TestGuardBurstIncludesSettledAnswersAndExpiresAtTenMinutes(t *testing.T) {
	f, a, b, _ := channel3(t)
	f.now = f.now.Truncate(time.Minute).Add(3 * time.Second)
	send := Ask{All: true, Each: true, Body: "independent work"}
	first := f.guardAsk(a, send, "burst-1")
	wantOK(t, f.reply(b, Reply{To: first.ID, Body: "done"}, "settled"))
	exchanges := map[int64]bool{first.Exchange: true}
	checkLevel := func(got AskResult) {
		t.Helper()
		if got.Round != 1 || !got.NewExchange || exchanges[got.Exchange] {
			t.Fatalf("independent request did not open a new thread at round 1: %+v", got)
		}
		exchanges[got.Exchange] = true
	}
	var workers sync.WaitGroup
	completed := make(chan core.Result, 12)
	for i := 2; i <= 13; i++ {
		workers.Go(func() { completed <- f.ask(a, send, fmt.Sprintf("burst-%d", i)) })
	}
	workers.Wait()
	close(completed)
	for result := range completed {
		wantOK(t, result)
		checkLevel(decode[AskResult](t, result))
	}
	for i := 14; i <= 30; i++ {
		checkLevel(f.guardAsk(a, send, fmt.Sprintf("burst-%d", i)))
	}
	before := f.state()
	r := f.ask(a, send, "over")
	wantCode(t, r, core.ExitRefused, core.CodeTooManyRequests)
	retry := f.now.Add(11 * time.Minute).Truncate(time.Minute)
	refusal := decode[Refusal](t, r)
	if refusal.Message != "not sent: 30 requests from you in 10 minutes" ||
		refusal.Hint != "it fits the burst limit again at "+retry.In(time.Local).Format("15:04")+". Nothing else lifts this." {
		t.Fatalf("burst refusal %+v", refusal)
	}
	again := f.guardAsk(a, send, "burst-1")
	if again.ID != first.ID || f.state() != before || len(exchanges) != 30 {
		t.Fatal("burst refusal or replay changed the ledger")
	}
	f.now = f.now.Add(10*time.Minute - time.Millisecond)
	wantCode(t, f.ask(a, send, "over"), core.ExitRefused, core.CodeTooManyRequests)
	f.now = retry
	executeGuardFixture(t, f, `UPDATE principal SET any_level_ms = ? WHERE id = ?`, ms(retry)+8*roundDurationMS, *f.caller(a).PrincipalID)
	wantCode(t, f.ask(a, send, "over"), core.ExitRefused, core.CodeRoundLimit)
	f.grantMember("form", 8)
	checkLevel(f.guardAsk(a, send, "over"))
}

func TestGuardPreservesOldReplayAndUnreadReply(t *testing.T) {
	f, a, b, _ := channel3(t)
	cmd := Ask{To: []string{"tax"}, Body: "before upgrade"}
	first := f.guardAsk(a, cmd, "saved")
	executeGuardFixture(t, f, `UPDATE message SET level_ms = 0 WHERE id = ?`, first.ID)
	executeGuardFixture(t, f, `UPDATE principal SET any_level_ms = ? WHERE id = ?`, ms(f.now)+8*roundDurationMS, *f.caller(a).PrincipalID)
	again := f.guardAsk(a, cmd, "saved")
	if again.ID != first.ID || f.messageLevel(first.ID) != 0 || f.count(`SELECT COUNT(*) FROM event WHERE kind = ?`, guardStopped) != 0 {
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
	f.grantMember("a11y", 8)
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
	sent := f.guardAsk(a, Ask{To: []string{"tax"}, Body: "before reset"}, "before")
	f.grantThread(sent.Exchange, 8)
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

func (f *fixture) stopEvent(subject string) guardStop {
	f.t.Helper()
	var raw []byte
	if err := f.db.QueryRow(`SELECT detail FROM event WHERE kind = ? AND subject = ? ORDER BY seq DESC LIMIT 1`, guardStopped, subject).Scan(&raw); err != nil {
		f.t.Fatal(err)
	}
	var out guardStop
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

func rounds(n int64) *int64 { return &n }

func pool(n int64) *int64 { return &n }

func (f *fixture) grantMember(alias string, n int64) {
	f.t.Helper()
	wantOK(f.t, f.owner(BudgetGrant{Channel: "checkout", Alias: alias, Rounds: rounds(n)}, ""))
}

func (f *fixture) grantThread(exchange, n int64) {
	f.t.Helper()
	wantOK(f.t, f.owner(BudgetGrant{Channel: "checkout", ExchangeID: exchange, Rounds: rounds(n)}, ""))
}
