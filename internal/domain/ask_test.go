package domain

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

// channel3 sets up #checkout with members form (a), tax (b) and a11y (c).
func channel3(t *testing.T) (*fixture, session, session, session) {
	f := newFixture(t)
	f.owner(CreateChannel{Name: "checkout"}, "")
	f.owner(CreateChannel{Name: "inventory"}, "")
	a, b, c := f.newSession("claude"), f.newSession("codex"), f.newSession("agy")
	wantOK(t, f.join(a, Join{Channel: "checkout", Alias: "form"}, ""))
	wantOK(t, f.join(b, Join{Channel: "checkout", Alias: "tax"}, ""))
	wantOK(t, f.join(c, Join{Channel: "checkout", Alias: "a11y"}, ""))
	return f, a, b, c
}

func (f *fixture) ask(s session, cmd Ask, key string) core.Result {
	return f.exec(f.caller(s), Request{OpKey: key, Command: cmd})
}

func (f *fixture) reply(s session, cmd Reply, key string) core.Result {
	return f.exec(f.caller(s), Request{OpKey: key, Command: cmd})
}

func (f *fixture) roots(s session, n int) {
	f.t.Helper()
	if _, err := f.db.Exec(`UPDATE principal SET root_tokens = root_tokens + ? WHERE id = ?`, n, *f.caller(s).PrincipalID); err != nil {
		f.t.Fatal(err)
	}
}

// pickUp records that s read message id, as the read command does.
func (f *fixture) pickUp(s session, id int64) {
	f.t.Helper()
	if _, err := f.db.Exec(`UPDATE recipient SET picked_up_at_ms = ? WHERE message_id = ? AND principal_id = ?`,
		f.now.UnixMilli(), id, *f.caller(s).PrincipalID); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) tokens(s session) int {
	return f.count(`SELECT root_tokens FROM principal WHERE id = ?`, *f.caller(s).PrincipalID)
}

// state is a fingerprint of everything an ask or a reply can write.
func (f *fixture) state() [8]int {
	var out [8]int
	for i, table := range []string{"message", "recipient", "obligation", "receipt", "exchange", "budget_event", "op", "event"} {
		out[i] = f.count(`SELECT COUNT(*) FROM ` + table)
	}
	return out
}

func TestNewMemberCannotOpenAnExchange(t *testing.T) {
	f, a, _, _ := channel3(t)
	before := f.state()
	r := f.ask(a, Ask{To: []string{"tax"}, Body: "review?"}, "k")
	wantCode(t, r, core.ExitRefused, core.CodeNoRootCredit)
	if hint := decode[Refusal](t, r).Hint; hint != "ask the owner: crosshall owner budget grant checkout/@form --roots 1" {
		t.Fatalf("hint %q", hint)
	}
	if f.state() != before {
		t.Fatal("a refused ask wrote something")
	}
}

func TestAskFansOutAtomically(t *testing.T) {
	f, a, b, c := channel3(t)
	f.roots(a, 1)
	before := f.state()
	for _, bad := range []Ask{
		{To: []string{"tax", "nobody"}, Body: "x"},
		{To: []string{"tax", "form"}, Body: "x"},
	} {
		r := f.ask(a, bad, "")
		wantCode(t, r, core.ExitRefused, core.CodeUnknownAlias)
	}
	if f.state() != before || f.tokens(a) != 1 {
		t.Fatal("a rejected fan-out wrote something or spent the token")
	}

	r := f.ask(a, Ask{To: []string{"@a11y", "tax", "tax"}, Body: "review refund rounding"}, "k1")
	wantOK(t, r)
	got := decode[AskResult](t, r)
	if got.Debit != 1 || got.Remaining != DefaultCredit-1 || !got.NewExchange || got.DueAtMS != f.now.Add(DefaultDue).UnixMilli() {
		t.Fatalf("result %+v", got)
	}
	if len(got.Recipients) != 2 || got.Recipients[0].Alias != "a11y" || got.Recipients[0].State != "stored" {
		t.Fatalf("recipients %+v", got.Recipients)
	}
	for q, want := range map[string]int{
		`SELECT COUNT(*) FROM message WHERE kind = 'ask'`:                               1,
		`SELECT COUNT(*) FROM recipient WHERE owes = 1`:                                 2,
		`SELECT COUNT(*) FROM obligation WHERE state = 'open'`:                          2,
		`SELECT COUNT(*) FROM receipt WHERE stage = 'stored'`:                           2,
		`SELECT granted FROM exchange`:                                                  DefaultCredit,
		`SELECT spent FROM exchange`:                                                    1,
		`SELECT COUNT(*) FROM exchange WHERE root_message_id = ` + itoa(got.ID):         1,
		`SELECT COUNT(*) FROM budget_event WHERE reason = 'initial' AND delta = 8`:      1,
		`SELECT COUNT(*) FROM budget_event WHERE reason = 'debit' AND delta = -1`:       1,
		`SELECT COUNT(*) FROM budget_event WHERE reason = 'roots' AND delta = -1`:       1,
		`SELECT COUNT(*) FROM binding WHERE alias = 'form' AND context_exchange_id = 1`: 1,
	} {
		if n := f.count(q); n != want {
			t.Errorf("%s = %d, want %d", q, n, want)
		}
	}
	if f.tokens(a) != 0 {
		t.Fatal("token not spent")
	}
	_ = b
	_ = c
}

func TestAskToTheWholeChannel(t *testing.T) {
	f, a, _, _ := channel3(t)
	f.roots(a, 1)
	wantCode(t, f.ask(a, Ask{All: true, Body: "status?"}, ""), core.ExitRefused, core.CodeEachRequired)
	got := decode[AskResult](t, f.ask(a, Ask{All: true, Each: true, Body: "status?"}, ""))
	var aliases []string
	for _, d := range got.Recipients {
		aliases = append(aliases, d.Alias)
	}
	if !reflect.DeepEqual(aliases, []string{"a11y", "tax"}) || got.Debit != 1 {
		t.Fatalf("audience %v debit %d", aliases, got.Debit)
	}
}

func TestAskRefusesDepartedAndForeignMembers(t *testing.T) {
	f, a, _, _ := channel3(t)
	f.roots(a, 1)
	d := f.newSession("route")
	wantOK(t, f.join(d, Join{Channel: "inventory", Alias: "stock"}, ""))
	wantCode(t, f.ask(a, Ask{To: []string{"stock"}, Body: "x"}, ""), core.ExitRefused, core.CodeUnknownAlias)
	if _, err := f.db.Exec(`UPDATE binding SET state = 'departed' WHERE alias = 'tax'`); err != nil {
		t.Fatal(err)
	}
	wantCode(t, f.ask(a, Ask{To: []string{"tax"}, Body: "x"}, ""), core.ExitRefused, core.CodeDeparted)
}

func TestFollowUpsStayInTheirExchange(t *testing.T) {
	f, a, b, c := channel3(t)
	f.roots(a, 1)
	f.roots(b, 1)
	x1 := decode[AskResult](t, f.ask(a, Ask{To: []string{"tax"}, Body: "q1"}, ""))

	// b owes in x1: its own ask is a follow-up there, and --new is refused.
	wantCode(t, f.ask(b, Ask{To: []string{"a11y"}, Body: "sub", New: true}, ""), core.ExitRefused, core.CodeMustAnswerFirst)
	sub := decode[AskResult](t, f.ask(b, Ask{To: []string{"a11y"}, Body: "sub"}, ""))
	if sub.Exchange != x1.Exchange || sub.NewExchange {
		t.Fatalf("follow-up went to x%d, want x%d", sub.Exchange, x1.Exchange)
	}
	if f.tokens(b) != 1 {
		t.Fatal("a follow-up spent a root token")
	}

	// After answering, the sticky context keeps b's next ask in x1 too.
	wantOK(t, f.reply(b, Reply{To: x1.ID, Body: "done"}, ""))
	again := decode[AskResult](t, f.ask(b, Ask{To: []string{"form"}, Body: "and?"}, ""))
	if again.Exchange != x1.Exchange {
		t.Fatalf("courtesy loop left x1 for x%d", again.Exchange)
	}

	// a owes nothing yet but its context is x1; an explicit --new opens x2
	// only with a token.
	wantOK(t, f.reply(a, Reply{To: again.ID, Body: "fine"}, ""))
	// Answered but never read: a new root waits until a reads it.
	wantCode(t, f.ask(a, Ask{To: []string{"tax"}, Body: "new task", New: true}, ""), core.ExitRefused, core.CodeMustAnswerFirst)
	f.pickUp(a, again.ID)
	wantCode(t, f.ask(a, Ask{To: []string{"tax"}, Body: "new task", New: true}, ""), core.ExitRefused, core.CodeNoRootCredit)
	f.roots(a, 1)
	x2 := decode[AskResult](t, f.ask(a, Ask{To: []string{"tax"}, Body: "new task", New: true}, ""))
	if !x2.NewExchange || x2.Exchange == x1.Exchange {
		t.Fatalf("--new stayed in x%d", x2.Exchange)
	}

	// c owes in x1 (sub); b now owes in x2 and asks c from there.
	wantOK(t, f.reply(b, Reply{To: x2.ID, Body: "on it", Outcome: OutcomeMoreTime}, ""))
	x2c := decode[AskResult](t, f.ask(b, Ask{To: []string{"a11y"}, Body: "help with x2"}, ""))
	if x2c.Exchange != x2.Exchange {
		t.Fatalf("b's follow-up went to x%d", x2c.Exchange)
	}
	r := f.ask(c, Ask{To: []string{"form"}, Body: "which?"}, "")
	wantCode(t, r, core.ExitRefused, core.CodeAmbiguousRoot)
	if hint := decode[Refusal](t, r).Hint; hint != "crosshall read "+itoa(sub.ID)+" "+itoa(x2c.ID) {
		t.Fatalf("unexpected owed request hint %q", hint)
	}
	via := decode[AskResult](t, f.ask(c, Ask{To: []string{"form"}, Body: "for x2", Re: x2c.ID}, ""))
	if via.Exchange != x2.Exchange {
		t.Fatalf("--re went to x%d", via.Exchange)
	}
}

func TestReRules(t *testing.T) {
	f, a, b, c := channel3(t)
	f.roots(a, 2)
	x1 := decode[AskResult](t, f.ask(a, Ask{To: []string{"tax"}, Body: "q1"}, ""))
	wantOK(t, f.reply(b, Reply{To: x1.ID, Body: "a1"}, ""))
	x2 := decode[AskResult](t, f.ask(a, Ask{To: []string{"a11y"}, Body: "q2", New: true}, ""))

	// Not a party to x1's root: c cannot attach to it.
	wantCode(t, f.ask(c, Ask{To: []string{"form"}, Body: "x", Re: x1.ID}, ""), core.ExitRefused, core.CodeBadRoot)
	wantCode(t, f.ask(c, Ask{To: []string{"form"}, Body: "x", Re: 999}, ""), core.ExitRefused, core.CodeNotFound)
	if got := decode[AskResult](t, f.ask(c, Ask{To: []string{"form"}, Body: "x", Re: x2.ID}, "")); got.Exchange != x2.Exchange {
		t.Fatalf("--re to x2 went to x%d", got.Exchange)
	}
	other := decode[AskResult](t, f.ask(a, Ask{To: []string{"tax"}, Body: "another x2 request", Re: x2.ID}, ""))
	wrong := f.ask(b, Ask{To: []string{"a11y"}, Body: "continue x1", Re: x1.ID}, "")
	wantCode(t, wrong, core.ExitRefused, core.CodeBadRoot)
	if hint := decode[Refusal](t, wrong).Hint; hint != "crosshall read "+itoa(other.ID) {
		t.Fatalf("wrong exchange refusal pointed to %q", hint)
	}
}

func TestBudgetExhaustionOpensOneEpisode(t *testing.T) {
	f, a, b, _ := channel3(t)
	f.roots(a, 1)
	first := decode[AskResult](t, f.ask(a, Ask{To: []string{"tax"}, Body: "q"}, ""))
	for i := 1; i < DefaultCredit; i++ {
		wantOK(t, f.ask(a, Ask{To: []string{"tax"}, Body: "again"}, ""))
	}
	for i := 0; i < 3; i++ {
		r := f.ask(a, Ask{To: []string{"tax"}, Body: "one more"}, "")
		wantCode(t, r, core.ExitRefused, core.CodeBudgetExhausted)
	}
	if n := f.count(`SELECT COUNT(*) FROM event WHERE kind = 'budget_exhausted'`); n != 1 {
		t.Fatalf("%d exhaustion events for one episode", n)
	}
	if n := f.count(`SELECT episode_open + episode_count FROM exchange`); n != 2 {
		t.Fatalf("episode state %d", n)
	}
	// Replies still work at zero.
	wantOK(t, f.reply(b, Reply{To: first.ID, Body: "answer"}, ""))
	wantOK(t, f.reply(b, Reply{To: first.ID + 1, Outcome: OutcomeDecline}, ""))

	// The owner closes the episode with a grant; the next exhaustion is a new episode.
	if _, err := f.db.Exec(`UPDATE exchange SET granted = granted + 1, episode_open = 0`); err != nil {
		t.Fatal(err)
	}
	wantOK(t, f.ask(a, Ask{To: []string{"tax"}, Body: "last"}, ""))
	wantCode(t, f.ask(a, Ask{To: []string{"tax"}, Body: "over"}, ""), core.ExitRefused, core.CodeBudgetExhausted)
	if n := f.count(`SELECT COUNT(*) FROM event WHERE kind = 'budget_exhausted'`); n != 2 {
		t.Fatalf("%d exhaustion events for two episodes", n)
	}
}

// With one unit left, a request to several members still fits: it spends the
// last unit, and only the next request is refused.
func TestAFanOutCanSpendTheLastUnit(t *testing.T) {
	f, a, _, _ := channel3(t)
	f.roots(a, 1)
	wantOK(t, f.ask(a, Ask{To: []string{"tax"}, Body: "q"}, ""))
	if _, err := f.db.Exec(`UPDATE exchange SET spent = granted - 1`); err != nil {
		t.Fatal(err)
	}
	got := decode[AskResult](t, f.ask(a, Ask{To: []string{"tax", "a11y"}, Body: "both"}, ""))
	if got.Debit != 1 || got.Remaining != 0 || len(got.Recipients) != 2 {
		t.Fatalf("result %+v", got)
	}
	wantCode(t, f.ask(a, Ask{To: []string{"tax"}, Body: "one"}, ""), core.ExitRefused, core.CodeBudgetExhausted)
}

func TestAskReplayShowsCurrentDelivery(t *testing.T) {
	f, a, b, _ := channel3(t)
	f.roots(a, 1)
	cmd := Ask{To: []string{"tax", "a11y"}, Body: "review"}
	first := decode[AskResult](t, f.ask(a, cmd, "k"))
	// tax picks it up after the response was lost.
	f.now = f.now.Add(time.Minute)
	pid := *f.caller(b).PrincipalID
	if _, err := f.db.Exec(`UPDATE recipient SET picked_up_at_ms = ? WHERE message_id = ? AND principal_id = ?`,
		f.now.UnixMilli(), first.ID, pid); err != nil {
		t.Fatal(err)
	}
	before := f.state()
	again := decode[AskResult](t, f.ask(a, Ask{To: []string{"a11y", "tax"}, Body: "review"}, "k"))
	if f.state() != before {
		t.Fatal("replay wrote something")
	}
	if again.ID != first.ID || again.Debit != first.Debit || again.DueAtMS != first.DueAtMS || again.Remaining != first.Remaining {
		t.Fatalf("frozen parts changed: %+v vs %+v", again, first)
	}
	if again.Recipients[1].Alias != "tax" || again.Recipients[1].State != "picked_up" {
		t.Fatalf("delivery not current: %+v", again.Recipients)
	}
	wantCode(t, f.ask(a, Ask{To: []string{"tax", "a11y"}, Body: "other"}, "k"), core.ExitRefused, core.CodeKeyConflict)
	wantCode(t, f.ask(a, Ask{To: []string{"tax", "a11y"}, Body: "review", Due: "1h"}, "k"), core.ExitRefused, core.CodeKeyConflict)
}

func TestReplyOutcomes(t *testing.T) {
	f, a, b, c := channel3(t)
	f.roots(a, 1)
	q := decode[AskResult](t, f.ask(a, Ask{To: []string{"tax", "a11y"}, Body: "review", Due: "10m"}, ""))
	bid, cid := *f.caller(b).PrincipalID, *f.caller(c).PrincipalID

	// More time: visible, obligation open, no settled receipt, same deadline.
	mt := decode[ReplyResult](t, f.reply(b, Reply{To: q.ID, Body: "need an hour", Outcome: OutcomeMoreTime}, ""))
	if mt.Obligation != "open" || mt.DueAtMS != q.DueAtMS || mt.To != "form" {
		t.Fatalf("more time %+v", mt)
	}
	if n := f.count(`SELECT COUNT(*) FROM receipt WHERE stage = 'settled'`); n != 0 {
		t.Fatal("more time wrote a settled receipt")
	}
	if n := f.count(`SELECT COUNT(*) FROM obligation WHERE principal_id = ? AND state = 'open' AND more_time_at_ms IS NOT NULL`, bid); n != 1 {
		t.Fatal("more time not recorded")
	}

	// A late answer settles with lateness; a second answer is refused; a
	// replay of the first returns its result.
	f.now = f.now.Add(15 * time.Minute)
	ans := decode[ReplyResult](t, f.reply(b, Reply{To: q.ID, Body: "looks right"}, "r1"))
	if ans.Obligation != "replied" || ans.LateByMS != (5*time.Minute).Milliseconds() {
		t.Fatalf("answer %+v", ans)
	}
	wantCode(t, f.reply(b, Reply{To: q.ID, Body: "again"}, ""), core.ExitRefused, core.CodeAlreadySettled)
	if r := decode[ReplyResult](t, f.reply(b, Reply{To: q.ID, Body: "looks right"}, "r1")); r.ID != ans.ID {
		t.Fatal("replay of a settled reply did not return the original")
	}
	for q2, want := range map[string]int{
		`SELECT COUNT(*) FROM receipt WHERE stage = 'settled' AND principal_id = ` + itoa(bid): 1,
		`SELECT COUNT(*) FROM recipient WHERE message_id = ` + itoa(ans.ID) + ` AND owes = 0`:  1,
		`SELECT COUNT(*) FROM message WHERE kind = 'reply'`:                                    2,
	} {
		if n := f.count(q2); n != want {
			t.Errorf("%s = %d, want %d", q2, n, want)
		}
	}

	// Decline needs no text; the owner's cancel leaves nothing to answer.
	wantOK(t, f.reply(c, Reply{To: q.ID, Outcome: OutcomeDecline}, ""))
	if n := f.count(`SELECT COUNT(*) FROM obligation WHERE principal_id = ? AND state = 'declined'`, cid); n != 1 {
		t.Fatal("decline not settled")
	}
	// A reply is not a request: nobody owes an answer to it.
	wantCode(t, f.reply(a, Reply{To: ans.ID, Body: "n/a", Outcome: OutcomeDecline}, ""), core.ExitRefused, core.CodeNotFound)
}

func TestReplyRefusals(t *testing.T) {
	f, a, b, c := channel3(t)
	f.roots(a, 1)
	q := decode[AskResult](t, f.ask(a, Ask{To: []string{"tax"}, Body: "review"}, ""))
	wantCode(t, f.reply(c, Reply{To: q.ID, Body: "me too"}, ""), core.ExitRefused, core.CodeNotObligated)
	wantCode(t, f.reply(b, Reply{To: 999, Body: "?"}, ""), core.ExitRefused, core.CodeNotFound)
	if _, err := f.db.Exec(`UPDATE obligation SET state = 'cancelled'`); err != nil {
		t.Fatal(err)
	}
	wantCode(t, f.reply(b, Reply{To: q.ID, Body: "late"}, ""), core.ExitRefused, core.CodeCancelled)
	wantCode(t, f.reply(b, Reply{To: q.ID}, ""), core.ExitUsage, "")

	d := f.newSession("route")
	wantOK(t, f.join(d, Join{Channel: "inventory"}, ""))
	wantCode(t, f.reply(d, Reply{To: q.ID, Body: "x"}, ""), core.ExitRefused, core.CodeNotFound)
}

func TestAskArgumentChecks(t *testing.T) {
	f, a, _, _ := channel3(t)
	f.roots(a, 1)
	for _, bad := range []Ask{
		{Body: "x"},
		{To: []string{"tax"}, All: true, Each: true, Body: "x"},
		{To: []string{"tax"}, Body: "  "},
		{To: []string{"tax"}, Body: "x", Due: "30s"},
		{To: []string{"tax"}, Body: "x", Due: "25h"},
		{To: []string{"tax"}, Body: "x", Due: "soon"},
		{To: []string{"tax"}, Body: "x", Re: 1, New: true},
		{To: []string{"Bad_Alias"}, Body: "x"},
	} {
		if r := f.ask(a, bad, ""); r.Status != core.ExitUsage {
			t.Errorf("%+v: status %d code %s", bad, r.Status, r.Code)
		}
	}
	// A multi-line body is fine.
	wantOK(t, f.ask(a, Ask{To: []string{"tax"}, Body: "line one\nline two", Due: "90s"}, ""))
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// Finding: an unread request, even one the owner cancelled, blocks a new root.
func TestUnreadCancelledAskBlocksNewRoot(t *testing.T) {
	f, a, b, _ := channel3(t)
	f.roots(a, 1)
	f.roots(b, 1)
	q := decode[AskResult](t, f.ask(a, Ask{To: []string{"tax"}, Body: "q"}, ""))
	if _, err := f.db.Exec(`UPDATE obligation SET state = 'cancelled'`); err != nil {
		t.Fatal(err)
	}
	before := f.state()
	r := f.ask(b, Ask{To: []string{"form"}, Body: "new", New: true}, "")
	wantCode(t, r, core.ExitRefused, core.CodeMustAnswerFirst)
	if f.state() != before || f.tokens(b) != 1 {
		t.Fatal("refused new root wrote something or spent the token")
	}
	f.pickUp(b, q.ID)
	if got := decode[AskResult](t, f.ask(b, Ask{To: []string{"form"}, Body: "new", New: true}, "")); !got.NewExchange {
		t.Fatalf("after reading: %+v", got)
	}
}

// Finding: invalid UTF-8 would be replaced during encoding, so two different
// byte strings could replay as one send.
func TestInvalidUTF8IsRefused(t *testing.T) {
	f, a, b, _ := channel3(t)
	f.roots(a, 1)
	before := f.state()
	if r := f.ask(a, Ask{To: []string{"tax"}, Body: "ok\xff"}, "k"); r.Status != core.ExitUsage {
		t.Fatalf("ask: status %d", r.Status)
	}
	q := decode[AskResult](t, f.ask(a, Ask{To: []string{"tax"}, Body: "q"}, ""))
	if r := f.reply(b, Reply{To: q.ID, Body: "fine\xfe"}, ""); r.Status != core.ExitUsage {
		t.Fatalf("reply: status %d", r.Status)
	}
	if n := f.count(`SELECT COUNT(*) FROM message`); n != before[0]+1 {
		t.Fatalf("%d messages: an invalid body was stored", n)
	}
}

// Finding: a reply result shows the reply's own delivery to the asker, as of
// the response, also when replayed.
func TestReplyShowsItsDeliveryToTheAsker(t *testing.T) {
	f, a, b, _ := channel3(t)
	f.roots(a, 1)
	q := decode[AskResult](t, f.ask(a, Ask{To: []string{"tax"}, Body: "q"}, ""))
	first := decode[ReplyResult](t, f.reply(b, Reply{To: q.ID, Body: "fine"}, "r"))
	if first.Delivery.Alias != "form" || first.Delivery.State != "stored" || first.Delivery.Next == "" {
		t.Fatalf("delivery on send %+v", first.Delivery)
	}
	f.now = f.now.Add(time.Minute)
	f.pickUp(a, first.ID)
	again := decode[ReplyResult](t, f.reply(b, Reply{To: q.ID, Body: "fine"}, "r"))
	if again.ID != first.ID || again.Obligation != "replied" || again.Delivery.State != "picked_up" ||
		again.Delivery.PickedUpAtMS != f.now.UnixMilli() {
		t.Fatalf("replay %+v", again)
	}
}

// Finding: a refused fan-out names every bad recipient with its reason.
func TestRefusedFanOutListsEveryBadTarget(t *testing.T) {
	f, a, _, _ := channel3(t)
	f.roots(a, 1)
	gone := f.newSession("route")
	wantOK(t, f.join(gone, Join{Channel: "checkout", Alias: "gone"}, ""))
	if _, err := f.db.Exec(`UPDATE binding SET state = 'departed' WHERE alias = 'gone'`); err != nil {
		t.Fatal(err)
	}
	before := f.state()
	r := f.ask(a, Ask{To: []string{"tax", "nobody", "gone", "form"}, Body: "x"}, "")
	wantCode(t, r, core.ExitRefused, core.CodeUnknownAlias)
	var got struct {
		Message        string
		InvalidTargets []InvalidTarget `json:"invalid_targets"`
	}
	if err := json.Unmarshal(r.Data, &got); err != nil {
		t.Fatal(err)
	}
	want := []InvalidTarget{{"form", "self"}, {"gone", "departed"}, {"nobody", "unknown"}}
	if !reflect.DeepEqual(got.InvalidTargets, want) {
		t.Fatalf("invalid targets %+v", got.InvalidTargets)
	}
	for _, s := range []string{"@form is you", "@gone left #checkout", "@nobody is not a member"} {
		if !strings.Contains(got.Message, s) {
			t.Errorf("message %q misses %q", got.Message, s)
		}
	}
	if f.state() != before {
		t.Fatal("a refused fan-out wrote something")
	}
	wantCode(t, f.ask(a, Ask{To: []string{"tax", "gone"}, Body: "x"}, ""), core.ExitRefused, core.CodeDeparted)
}
