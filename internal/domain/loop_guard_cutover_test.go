package domain

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

const stopGuidance = "do not retry or reroute it. crosshall reply and crosshall note still work. The stop is recorded for the owner."

func TestGuardStopsThreeKindsOfLoopsAtRoundNine(t *testing.T) {
	for _, mode := range []string{"owed", "plain", "reply"} {
		t.Run(mode, func(t *testing.T) {
			f, a, b, _ := channel3(t)
			sender, recipient := a, b
			send := Ask{To: []string{"tax"}, Body: "start"}
			last := f.guardAsk(sender, send, "1")
			subject := fmt.Sprintf("exchange:%d", last.Exchange)
			message := fmt.Sprintf("not sent: this request would be round 9 on x%d; this thread may run 8 rounds ahead", last.Exchange)
			for round := int64(2); round <= 9; round++ {
				if mode == "owed" || mode == "plain" {
					sender, recipient = recipient, sender
				}
				to := "tax"
				if sender == b {
					to = "form"
				}
				send = Ask{To: []string{to}, Body: "continue"}
				switch mode {
				case "plain":
					wantOK(t, f.run(sender, Read{IDs: []int64{last.ID}}, ""))
					wantOK(t, f.reply(sender, Reply{To: last.ID, Body: "answer"}, ""))
				case "reply":
					r := f.reply(b, Reply{To: last.ID, Body: "answer"}, "")
					wantOK(t, r)
					reply := decode[ReplyResult](t, r)
					wantOK(t, f.run(a, Read{IDs: []int64{reply.ID}}, ""))
					send.Re = reply.ID
				}
				if round < 9 {
					last = f.guardAsk(sender, send, fmt.Sprint(round))
					if last.Round != round {
						t.Fatalf("step %d: %+v", round, last)
					}
					continue
				}
				if mode == "plain" {
					subject = fmt.Sprintf("principal:%d", *f.caller(sender).PrincipalID)
					message = "not sent: this request would be round 9 across what you have picked up; new threads from you may run 8 rounds ahead"
				}
			}
			before := f.count(`SELECT COUNT(*) FROM message`)
			for i := 0; i < 10; i++ {
				r := f.ask(sender, send, fmt.Sprintf("stop-%d", i))
				wantCode(t, r, core.ExitRefused, core.CodeRoundLimit)
				if got := decode[Refusal](t, r); got.Message != message || got.Hint != stopGuidance {
					t.Fatalf("refusal: %+v", got)
				}
			}
			if f.count(`SELECT COUNT(*) FROM message`) != before || f.count(`SELECT COUNT(*) FROM event WHERE kind = ?`, guardStopped) != 1 {
				t.Fatal("refused requests were stored or duplicated the stop")
			}
			stop := f.stopEvent(subject)
			if stop.Channel != "checkout" || stop.Sender != "form" || stop.Round != 9 || stop.RoundLimit != 8 ||
				len(stop.Recipients) != 1 || stop.Recipients[0] != "tax" {
				t.Fatalf("stop detail: %+v", stop)
			}
		})
	}
}

func TestGuardStopRearmsOnlyAfterThePathDrains(t *testing.T) {
	f, a, _, _ := channel3(t)
	parent := f.guardAsk(a, Ask{To: []string{"tax"}, Body: "deep"}, "")
	ask := Ask{Re: parent.ID, To: []string{"tax"}, Body: "follow up"}
	setDeep := func() {
		executeGuardFixture(t, f, `UPDATE message SET level_ms = ? WHERE id = ?`, ms(f.now)+8*roundDurationMS, parent.ID)
	}
	setDeep()
	wantCode(t, f.ask(a, ask, ""), core.ExitRefused, core.CodeRoundLimit)
	f.now = f.now.Add(10 * time.Minute)
	if got := f.guardAsk(a, ask, ""); got.Round != 8 || f.count(`SELECT episode_open FROM exchange WHERE id = ?`, parent.Exchange) != 1 {
		t.Fatalf("a partially drained request changed the stop: %+v", got)
	}
	f.now = f.now.Add(70 * time.Minute)
	if got := f.guardAsk(a, ask, ""); got.Round != 1 || f.count(`SELECT episode_open FROM exchange WHERE id = ?`, parent.Exchange) != 0 {
		t.Fatalf("a drained path did not reopen: %+v", got)
	}
	setDeep()
	wantCode(t, f.ask(a, ask, ""), core.ExitRefused, core.CodeRoundLimit)
	if f.count(`SELECT episode_count FROM exchange WHERE id = ?`, parent.Exchange) != 2 || f.count(`SELECT COUNT(*) FROM event WHERE kind = ?`, guardStopped) != 2 {
		t.Fatal("a new stop was not recorded after draining")
	}
}

func TestGuardAnyPathHintNamesOnlyPassingOwedThreads(t *testing.T) {
	f, a, b, c := channel3(t)
	shallow := f.guardAsk(a, Ask{To: []string{"tax"}, Body: "fresh"}, "")
	deep := f.guardAsk(c, Ask{To: []string{"tax"}, Body: "deep"}, "")
	executeGuardFixture(t, f, `UPDATE message SET level_ms = ? WHERE id = ?`, ms(f.now)+8*roundDurationMS, deep.ID)
	cmd := Ask{To: []string{"form"}, Body: "delegate"}
	refused := f.ask(b, cmd, "")
	wantCode(t, refused, core.ExitRefused, core.CodeRoundLimit)
	want := fmt.Sprintf("%s If it follows a request you owe, add --re <id>: %d.", stopGuidance, shallow.ID)
	if got := decode[Refusal](t, refused); got.Hint != want {
		t.Fatalf("hint: got %q, want %q", got.Hint, want)
	}
	cmd.Re = shallow.ID
	if got := f.guardAsk(b, cmd, ""); got.Round != 2 || got.Exchange != shallow.Exchange {
		t.Fatalf("hint named a path that could not accept: %+v", got)
	}
}

func TestGuardShallowLegacySiblingDoesNotRearmAStillStoppedRoot(t *testing.T) {
	f, a, _, _ := channel3(t)
	deep := f.guardAsk(a, Ask{To: []string{"tax"}, Body: "deep"}, "")
	sibling := f.guardAsk(a, Ask{To: []string{"tax"}, Body: "shallow"}, "")
	executeGuardFixture(t, f, `UPDATE message SET level_ms = ? WHERE id = ?`, ms(f.now)+8*roundDurationMS, deep.ID)
	executeGuardFixture(t, f, `UPDATE message SET exchange_id = ?, level_ms = ? WHERE id = ?`, deep.Exchange, ms(f.now), sibling.ID)
	cmd := Ask{Re: deep.ID, To: []string{"tax"}, Body: "follow up"}
	wantCode(t, f.ask(a, cmd, ""), core.ExitRefused, core.CodeRoundLimit)
	if sent := f.guardAsk(a, Ask{Re: sibling.ID, To: []string{"tax"}, Body: "sibling follow-up"}, ""); sent.Round != 1 {
		t.Fatalf("shallow root in the old exchange was not usable: %+v", sent)
	}
	wantCode(t, f.ask(a, cmd, ""), core.ExitRefused, core.CodeRoundLimit)
	if f.count(`SELECT COUNT(*) FROM event WHERE kind = ?`, guardStopped) != 1 {
		t.Fatal("a shallow legacy sibling rearmed a still-deep root")
	}
}

func TestGuardAnswerBurstAndOversizedAudienceWriteNothing(t *testing.T) {
	f, a, _, _ := channel3(t)
	audience := []string{"tax", "a11y"}
	for i := 0; i < 59; i++ {
		alias := fmt.Sprintf("reader-%d", i)
		session := f.newSession("route")
		wantOK(t, f.join(session, Join{Channel: "checkout", Alias: alias}, ""))
		audience = append(audience, alias)
	}
	before := f.state()
	r := f.ask(a, Ask{All: true, Each: true, Body: "oversized"}, "")
	wantCode(t, r, core.ExitRefused, core.CodeAudienceTooLarge)
	if got := decode[Refusal](t, r); got.Message != "not sent: 61 members would owe an answer; one request may ask at most 60" ||
		got.Hint != "name fewer members, or send a note." || f.state() != before {
		t.Fatalf("oversized refusal or state: %+v", got)
	}
	f.guardAsk(a, Ask{To: audience[:58], Body: "58 answers"}, "")
	before = f.state()
	r = f.ask(a, Ask{To: audience[:8], Body: "8 more"}, "")
	wantCode(t, r, core.ExitRefused, core.CodeTooManyRequests)
	if got := decode[Refusal](t, r); got.Message != "not sent: your requests in the last 10 minutes already ask for 58 answers; this one adds 8 and the limit is 60" ||
		!strings.Contains(got.Hint, "Nothing else lifts this.") || f.state() != before {
		t.Fatalf("answer refusal or state: %+v", got)
	}
}

func TestFreshJoinStartsWithoutReadingOrSpendingLegacyCredit(t *testing.T) {
	f, a, _, _ := channel3(t)
	executeGuardFixture(t, f, `UPDATE channel SET join_pool = 9 WHERE name = 'checkout'`)
	executeGuardFixture(t, f, `UPDATE principal SET any_level_ms = ?, root_tokens = 4 WHERE id = ?`,
		ms(f.now)+8*roundDurationMS, *f.caller(a).PrincipalID)
	fresh := f.newSession("route")
	wantOK(t, f.join(fresh, Join{Channel: "checkout", Alias: "fresh"}, "join"))
	got := f.guardAsk(fresh, Ask{To: []string{"form"}, Body: "a first request"}, "first")
	if got.Round != 1 || !got.NewExchange || f.count(`SELECT join_pool FROM channel WHERE name = 'checkout'`) != 9 ||
		f.count(`SELECT COUNT(*) FROM budget_event`) != 0 {
		t.Fatalf("fresh member depends on the old budget or a peer's position: %+v", got)
	}
}

func TestGuardCutoverLeavesOldWorkAndKeysUsable(t *testing.T) {
	for _, recorded := range []bool{false, true} {
		t.Run(fmt.Sprintf("recorded=%t", recorded), func(t *testing.T) {
			f, a, b, c := channel3(t)
			saved := Ask{To: []string{"tax"}, Body: "pickup after upgrade"}
			first := f.guardAsk(a, saved, "old-key")
			second := f.guardAsk(a, Ask{To: []string{"a11y"}, Body: "reply after upgrade"}, "")
			principal := *f.caller(a).PrincipalID
			level := int64(0)
			if recorded {
				level = ms(f.now) + 8*roundDurationMS
				executeGuardFixture(t, f, `INSERT INTO thread_position VALUES (?, ?, ?)`, principal, first.ID, level)
				executeGuardFixture(t, f, `UPDATE principal SET guard_told = 1, any_level_ms = ? WHERE id = ?`, level, principal)
			}
			executeGuardFixture(t, f, `UPDATE message SET level_ms = ? WHERE id IN (?, ?)`, level, first.ID, second.ID)
			executeGuardFixture(t, f, `UPDATE exchange SET granted = 8, spent = 8, episode_open = 1, episode_count = 3`)
			executeGuardFixture(t, f, `UPDATE op SET result = replace(result, '"round":1', '"debit":1,"remaining":0') WHERE op_key = 'old-key'`)
			migration, err := os.ReadFile("../store/migrations/022_loop_guard_cutover.sql")
			if err != nil {
				t.Fatal(err)
			}
			executeGuardFixture(t, f, string(migration))
			if replay := f.guardAsk(a, saved, "old-key"); replay.ID != first.ID || replay.Round != 0 {
				t.Fatalf("old replay: %+v", replay)
			}
			wantOK(t, f.run(b, Read{IDs: []int64{first.ID}}, ""))
			answer := f.reply(c, Reply{To: second.ID, Body: "finished"}, "")
			wantOK(t, answer)
			wantOK(t, f.run(a, Read{IDs: []int64{decode[ReplyResult](t, answer).ID}}, ""))
			if f.anyGuardLevel(a) != 0 || f.anyGuardLevel(b) != 0 || f.anyGuardLevel(c) != 0 ||
				f.count(`SELECT COUNT(*) FROM thread_position`) != 0 {
				t.Fatal("old levels crossed the cutover fence")
			}
			next := Ask{Re: first.ID, To: []string{"tax"}, Body: "continued"}
			sent := f.guardAsk(a, next, "")
			if sent.Round != 1 || sent.Exchange != first.Exchange {
				t.Fatalf("old exhausted exchange could not continue: %+v", sent)
			}
			executeGuardFixture(t, f, `UPDATE message SET level_ms = ? WHERE id = ?`, ms(f.now)+8*roundDurationMS, sent.ID)
			next.Re = sent.ID
			wantCode(t, f.ask(a, next, ""), core.ExitRefused, core.CodeRoundLimit)
			if f.count(`SELECT episode_count FROM exchange WHERE id = ?`, first.Exchange) != 1 ||
				f.count(`SELECT COUNT(*) FROM event WHERE kind = ?`, guardStopped) != 1 {
				t.Fatal("the first genuine stop was hidden by a legacy stop")
			}
		})
	}
}
