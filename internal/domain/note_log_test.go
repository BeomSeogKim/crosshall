package domain

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

func (f *fixture) run(s session, cmd Command, key string) core.Result {
	return f.exec(f.caller(s), Request{OpKey: key, Command: cmd, Vendor: s.vendor})
}

// resolved is the caller the dispatcher builds: a member only while a live
// binding exists, otherwise unenrolled with its principal.
func (f *fixture) resolved(s session) core.Caller {
	c := f.caller(s)
	if c.PrincipalID != nil &&
		f.count(`SELECT COUNT(*) FROM binding WHERE principal_id = ? AND state = 'live'`, *c.PrincipalID) == 0 {
		c.Role = core.RoleUnenrolled
	}
	return c
}

// budget is everything a note must not touch.
func (f *fixture) budget() [4]int {
	return [4]int{
		f.count(`SELECT COUNT(*) FROM obligation`),
		f.count(`SELECT COUNT(*) FROM exchange`),
		f.count(`SELECT COUNT(*) FROM budget_event`),
		f.count(`SELECT COALESCE(SUM(root_tokens), 0) FROM principal`),
	}
}

func deliveryAliases(ds []Delivery) []string {
	var out []string
	for _, d := range ds {
		out = append(out, d.Alias)
	}
	return out
}

// A note to the whole channel: the audience is the live members minus the sender; nobody
// owes anything, no budget moves, and each recipient picks it up on its own.
func TestNoteToTheWholeChannel(t *testing.T) {
	f, a, b, c := channel3(t)
	d := f.newSession("claude")
	wantOK(t, f.join(d, Join{Channel: "checkout", Alias: "gone"}, ""))
	wantOK(t, f.run(d, Leave{}, "leave-d"))
	f.roots(a, 1)
	before := f.budget()

	r := f.run(a, Note{All: true, Body: "deploy freeze at 17:00"}, "n1")
	wantOK(t, r)
	note := decode[NoteResult](t, r)
	if got := deliveryAliases(note.Recipients); !reflect.DeepEqual(got, []string{"a11y", "tax"}) {
		t.Fatalf("audience %v", got)
	}
	if f.budget() != before {
		t.Fatalf("a note changed obligations or budget: %v -> %v", before, f.budget())
	}
	if n := f.count(`SELECT COUNT(*) FROM recipient WHERE message_id = ? AND owes = 1`, note.ID); n != 0 {
		t.Fatalf("%d recipients owe an answer", n)
	}
	if mode := f.count(`SELECT COUNT(*) FROM message WHERE id = ? AND kind = 'note' AND audience_mode = 'all' AND exchange_id IS NULL`, note.ID); mode != 1 {
		t.Fatal("note row")
	}

	wantOK(t, f.run(b, Read{IDs: []int64{note.ID}}, "rb"))
	inbox := decode[InboxResult](t, f.run(c, Inbox{}, ""))
	if len(inbox.Unread) != 1 || inbox.Unread[0].ID != note.ID {
		t.Fatalf("c's pickup must not depend on b's: %+v", inbox)
	}
	if bi := decode[InboxResult](t, f.run(b, Inbox{}, "")); len(bi.Unread)+len(bi.Owed) != 0 {
		t.Fatalf("a read note stays in b's inbox: %+v", bi)
	}
	if w := decode[WaitingResult](t, f.run(a, Waiting{}, "")); len(w.Requests) != 0 {
		t.Fatalf("a note is never waited on: %+v", w)
	}
	states := map[string]string{}
	for _, d := range decode[NoteResult](t, f.run(a, Note{All: true, Body: "deploy freeze at 17:00"}, "n1")).Recipients {
		states[d.Alias] = d.State
	}
	if states["tax"] != "picked_up" || states["a11y"] != "stored" {
		t.Fatalf("replay shows current delivery: %v", states)
	}
}

func TestNoteRefusalsWriteNothing(t *testing.T) {
	f, a, _, _ := channel3(t)
	d := f.newSession("claude")
	wantOK(t, f.join(d, Join{Channel: "checkout", Alias: "gone"}, ""))
	wantOK(t, f.run(d, Leave{}, ""))
	before := f.state()

	r := f.run(a, Note{To: []string{"tax", "nobody", "gone", "form"}, Body: "fyi"}, "k1")
	wantCode(t, r, core.ExitRefused, core.CodeUnknownAlias)
	if msg := decode[Refusal](t, r).Message; !strings.Contains(msg, "@nobody") || !strings.Contains(msg, "@gone") || !strings.Contains(msg, "@form is you") {
		t.Fatalf("every bad target is named: %q", msg)
	}
	for _, bad := range []Note{
		{Body: "no target"},
		{To: []string{"tax"}, All: true, Body: "both"},
		{To: []string{"tax"}, Body: "  "},
		{To: []string{"tax"}, Body: "\xff"},
		{To: []string{"tax"}, Body: strings.Repeat("x", MaxBody+1)},
	} {
		wantCode(t, f.run(a, bad, "k2"), core.ExitUsage, "")
	}
	if f.state() != before {
		t.Fatal("a refused note wrote something")
	}

	wantOK(t, f.run(a, Note{To: []string{"tax", "a11y"}, Body: "fyi"}, "k3"))
	same := f.run(a, Note{To: []string{"a11y", "tax", "tax"}, Body: "fyi"}, "k3")
	wantOK(t, same)
	wantCode(t, f.run(a, Note{To: []string{"tax", "a11y"}, Body: "fyi!"}, "k3"), core.ExitRefused, core.CodeKeyConflict)
	if n := f.count(`SELECT COUNT(*) FROM message WHERE kind = 'note'`); n != 1 {
		t.Fatalf("%d notes after a replay", n)
	}
}

// Channels do not leak: a member of another channel is absent from who and log, and
// cannot be addressed or read by name or by ID, even with the same alias.
func TestChannelsDoNotLeak(t *testing.T) {
	f, a, b, _ := channel3(t)
	c := f.newSession("codex")
	other := f.newSession("claude")
	wantOK(t, f.join(c, Join{Channel: "inventory", Alias: "stock"}, ""))
	wantOK(t, f.join(other, Join{Channel: "inventory", Alias: "tax"}, ""))
	secret := decode[NoteResult](t, f.run(c, Note{To: []string{"tax"}, Body: "inventory secret"}, "s"))
	wantOK(t, f.run(a, Note{To: []string{"tax"}, Body: "checkout note"}, "n"))

	if got := aliases(decode[WhoResult](t, f.run(a, Who{}, "")).Members); strings.Contains(strings.Join(got, ","), "stock") {
		t.Fatalf("who leaks: %v", got)
	}
	log := decode[LogResult](t, f.run(a, Log{}, ""))
	for _, e := range log.Messages {
		if e.ID == secret.ID || strings.Contains(e.Body, "secret") || e.Sender == "stock" {
			t.Fatalf("log leaks: %+v", e)
		}
	}
	wantCode(t, f.run(a, Note{To: []string{"stock"}, Body: "hi"}, ""), core.ExitRefused, core.CodeUnknownAlias)
	wantCode(t, f.run(a, Read{IDs: []int64{secret.ID}}, "r"), core.ExitRefused, core.CodeNotFound)
	// @tax in #checkout is not @tax in #inventory.
	if got := decode[InboxResult](t, f.run(b, Inbox{}, "")); len(got.Unread) != 1 || got.Unread[0].ID == secret.ID {
		t.Fatalf("b sees another channel's message: %+v", got)
	}
}

func TestLogPagesAndRecordsNoPickup(t *testing.T) {
	f, a, b, _ := channel3(t)
	f.roots(a, 1)
	var ids []int64
	for i := 0; i < 5; i++ {
		ids = append(ids, decode[NoteResult](t, f.run(a, Note{To: []string{"tax"}, Body: "n" + itoa(int64(i))}, "")).ID)
	}
	ask := decode[AskResult](t, f.ask(a, Ask{To: []string{"tax"}, Body: "ok?\nsecond line"}, "ask"))
	ids = append(ids, ask.ID)
	f.now = f.now.Add(time.Minute)
	f.pickUp(b, ask.ID)
	rep := decode[ReplyResult](t, f.reply(b, Reply{To: ask.ID, Body: "yes"}, "rep"))
	ids = append(ids, rep.ID)
	before := f.count(`SELECT COUNT(*) FROM recipient WHERE picked_up_at_ms IS NOT NULL`)

	latest := decode[LogResult](t, f.run(b, Log{Limit: 3}, ""))
	if got := logIDs(latest); !reflect.DeepEqual(got, ids[4:]) || latest.Earlier != 4 || latest.Later != 0 {
		t.Fatalf("latest page %v earlier %d later %d", got, latest.Earlier, latest.Later)
	}
	if latest.Next == nil || *latest.Next != 0 {
		t.Fatalf("older history continues from the start: %v", latest.Next)
	}
	back := decode[LogResult](t, f.run(b, Log{Since: latest.Next, Limit: 3}, ""))
	if got := logIDs(back); !reflect.DeepEqual(got, ids[:3]) || back.Later != 4 || *back.Next != ids[2] {
		t.Fatalf("first page %v later %d next %v", got, back.Later, back.Next)
	}
	zero := int64(0)
	if got := logIDs(decode[LogResult](t, f.run(b, Log{Since: &zero}, ""))); !reflect.DeepEqual(got, ids) {
		t.Fatalf("from the start %v", got)
	}
	last := latest.Messages[2]
	if last.Kind != "reply" || last.Sender != "tax" || !reflect.DeepEqual(last.To, []string{"form"}) ||
		last.InReplyTo == nil || *last.InReplyTo != ask.ID || last.Body != "yes" {
		t.Fatalf("reply entry %+v", last)
	}
	if e := latest.Messages[1]; e.DueAtMS == nil || e.Exchange == nil || e.Body != "ok?\nsecond line" {
		t.Fatalf("ask entry %+v", e)
	}
	if after := f.count(`SELECT COUNT(*) FROM recipient WHERE picked_up_at_ms IS NOT NULL`); after != before {
		t.Fatal("log recorded a pickup")
	}
	for _, bad := range []Log{{Limit: -1}, {Limit: MaxLogLimit + 1}} {
		wantCode(t, f.run(b, bad, ""), core.ExitUsage, "")
	}
}

func logIDs(r LogResult) []int64 {
	var out []int64
	for _, e := range r.Messages {
		out = append(out, e.ID)
	}
	return out
}

// Leave: obligations survive a leave, another
// channel stays refused, and coming back restores the alias.
func TestLeaveKeepsObligationsAndOneChannel(t *testing.T) {
	f, a, b, _ := channel3(t)
	f.roots(a, 1)
	ask := decode[AskResult](t, f.ask(a, Ask{To: []string{"tax"}, Body: "review?"}, "ask"))

	r := f.exec(f.resolved(b), Request{OpKey: "bye", Command: Leave{}})
	wantOK(t, r)
	left := decode[LeaveResult](t, r)
	if left.Alias != "tax" || left.Owed != 1 || left.Waiting != 0 {
		t.Fatalf("leave %+v", left)
	}
	if n := f.count(`SELECT COUNT(*) FROM obligation WHERE message_id = ? AND state = 'open'`, ask.ID); n != 1 {
		t.Fatal("a leave settled an obligation")
	}
	if w := decode[WaitingResult](t, f.run(a, Waiting{}, "")); len(w.Requests) != 1 {
		t.Fatalf("the asker still waits: %+v", w)
	}
	changes := decode[WhoResult](t, f.run(a, Who{}, ""))
	if strings.Contains(strings.Join(aliases(changes.Members), ","), "tax") {
		t.Fatal("a departed member is listed")
	}

	// A lost response: the retry arrives from an unenrolled caller.
	wantOK(t, f.exec(f.resolved(b), Request{OpKey: "bye", Command: Leave{}}))
	wantCode(t, f.exec(f.resolved(b), Request{OpKey: "again", Command: Leave{}}), core.ExitNotEnrolled, core.CodeNotEnrolled)
	wantCode(t, f.exec(f.resolved(b), Request{Command: Inbox{}}), core.ExitNotEnrolled, core.CodeNotEnrolled)
	wantCode(t, f.exec(f.resolved(b), Request{Command: Reply{To: ask.ID, Body: "late"}}), core.ExitNotEnrolled, core.CodeNotEnrolled)
	if n := f.count(`SELECT COUNT(*) FROM roster_change WHERE field = 'departed'`); n != 1 {
		t.Fatalf("%d departures recorded", n)
	}

	wantCode(t, f.join(b, Join{Channel: "inventory"}, ""), core.ExitRefused, core.CodeOneChannel)
	back := decode[JoinResult](t, f.join(b, Join{Channel: "checkout"}, ""))
	if back.Alias != "tax" {
		t.Fatalf("re-join %+v", back)
	}
	wantOK(t, f.reply(b, Reply{To: ask.ID, Body: "back, done"}, "rep"))
}

func TestFocus(t *testing.T) {
	f, a, b, _ := channel3(t)
	seq := f.count(`SELECT MAX(seq) FROM roster_change`)
	r := decode[FocusResult](t, f.run(a, Focus{Text: "refund rounding"}, "f1"))
	if !r.Changed || r.Focus != "refund rounding" {
		t.Fatalf("focus %+v", r)
	}
	for _, m := range decode[WhoResult](t, f.run(b, Who{}, "")).Members {
		if m.Alias == "form" && m.Focus != "refund rounding" {
			t.Fatalf("who shows %q", m.Focus)
		}
	}
	if again := decode[FocusResult](t, f.run(a, Focus{Text: "refund rounding"}, "f2")); again.Changed {
		t.Fatal("the same focus counted as a change")
	}
	if n := f.count(`SELECT MAX(seq) FROM roster_change`); n != seq+1 {
		t.Fatalf("roster changes %d -> %d", seq, n)
	}
	wantCode(t, f.run(a, Focus{Text: "two\nlines"}, "f3"), core.ExitUsage, "")
	wantCode(t, f.run(a, Focus{Text: " "}, "f4"), core.ExitUsage, "")
	wantCode(t, f.run(a, Focus{Text: "other"}, "f1"), core.ExitRefused, core.CodeKeyConflict)
}

// A reply after the deadline keeps the deadline, the
// settlement time and the lateness in the owner's ledger.
func TestLateReplyStaysVisible(t *testing.T) {
	f, a, b, _ := channel3(t)
	f.roots(a, 1)
	ask := decode[AskResult](t, f.ask(a, Ask{To: []string{"tax"}, Body: "review?", Due: "1m"}, "ask"))
	f.pickUp(b, ask.ID)
	f.now = f.now.Add(90 * time.Second)
	settled := f.now.UnixMilli()
	rep := decode[ReplyResult](t, f.reply(b, Reply{To: ask.ID, Body: "sorry, late"}, "rep"))
	if rep.LateByMS != 30_000 || rep.DueAtMS != ask.DueAtMS {
		t.Fatalf("reply %+v", rep)
	}
	f.now = f.now.Add(time.Hour)
	status := decode[StatusResult](t, f.owner(OwnerStatus{}, ""))
	var found bool
	for _, ch := range status.Channels {
		for _, req := range ch.Requests {
			if req.ID != ask.ID {
				continue
			}
			found = true
			r := req.Recipients[0]
			if req.DueAtMS != ask.DueAtMS || r.State != "replied" || r.SettledAtMS == nil || *r.SettledAtMS != settled ||
				r.LateByMS == nil || *r.LateByMS != 30_000 || r.OverdueSinceMS != nil {
				t.Fatalf("ledger row %+v", r)
			}
		}
	}
	if !found {
		t.Fatal("the late request is missing from the owner ledger")
	}
}

// A departed member whose alias was taken meanwhile comes back under a new,
// free alias and keeps its own history; other alias changes stay refused.
func TestReturnUnderANewAliasWhenTheOldOneIsTaken(t *testing.T) {
	f, a, b, _ := channel3(t)
	f.roots(a, 1)
	ask := decode[AskResult](t, f.ask(a, Ask{To: []string{"tax"}, Body: "review?"}, "ask"))
	wantOK(t, f.run(b, Leave{}, "bye"))
	successor := f.newSession("codex")
	wantOK(t, f.join(successor, Join{Channel: "checkout", Alias: "tax"}, ""))

	r := f.join(b, Join{Channel: "checkout"}, "")
	wantCode(t, r, core.ExitRefused, core.CodeAliasTaken)
	if hint := decode[Refusal](t, r).Hint; hint != "crosshall join checkout --as <another alias>" {
		t.Fatalf("hint %q", hint)
	}
	wantCode(t, f.join(b, Join{Channel: "checkout", Alias: "form"}, ""), core.ExitRefused, core.CodeAliasTaken)
	back := decode[JoinResult](t, f.join(b, Join{Channel: "checkout", Alias: "tax2"}, ""))
	if back.Alias != "tax2" {
		t.Fatalf("return %+v", back)
	}
	// The obligation follows the binding, not the alias text.
	wantCode(t, f.reply(successor, Reply{To: ask.ID, Body: "not mine"}, ""), core.ExitRefused, core.CodeNotObligated)
	wantOK(t, f.reply(b, Reply{To: ask.ID, Body: "done"}, "rep"))
	if n := f.count(`SELECT COUNT(*) FROM binding WHERE principal_id = ?`, *f.caller(b).PrincipalID); n != 1 {
		t.Fatalf("%d bindings for the returning member", n)
	}

	// A live member, or a departed one whose alias is free, cannot rename.
	wantCode(t, f.join(b, Join{Channel: "checkout", Alias: "tax3"}, ""), core.ExitRefused, core.CodeAliasTaken)
	wantOK(t, f.run(a, Leave{}, ""))
	wantCode(t, f.join(a, Join{Channel: "checkout", Alias: "form2"}, ""), core.ExitRefused, core.CodeAliasTaken)
	if got := decode[JoinResult](t, f.join(a, Join{Channel: "checkout"}, "")); got.Alias != "form" {
		t.Fatalf("return with a free alias %+v", got)
	}
}

// Following every continuation from the latest page reaches every message
// exactly once.
func TestLogContinuationsReachEveryMessage(t *testing.T) {
	f, a, b, _ := channel3(t)
	var want []int64
	for i := 0; i < 7; i++ {
		want = append(want, decode[NoteResult](t, f.run(a, Note{To: []string{"tax"}, Body: "n" + itoa(int64(i))}, "")).ID)
	}
	seen := map[int64]int{}
	page := decode[LogResult](t, f.run(b, Log{Limit: 3}, ""))
	for pages := 1; ; pages++ {
		for _, id := range logIDs(page) {
			seen[id]++
		}
		if page.Next == nil {
			if pages < 3 {
				t.Fatalf("only %d pages", pages)
			}
			break
		}
		if pages > 10 {
			t.Fatal("continuations loop")
		}
		page = decode[LogResult](t, f.run(b, Log{Since: page.Next, Limit: 3}, ""))
	}
	for _, id := range want {
		if seen[id] == 0 {
			t.Fatalf("#%d never shown: %v", id, seen)
		}
	}
}

// A page never outgrows the response the transport accepts; the rest is a
// continuation, with every body whole.
func TestLogPageFitsTheTransport(t *testing.T) {
	f, a, b, _ := channel3(t)
	body := strings.Repeat("<", MaxBody) // escaped six bytes per character in JSON
	var want []int64
	for i := 0; i < 60; i++ {
		want = append(want, decode[NoteResult](t, f.run(a, Note{To: []string{"tax"}, Body: body}, "")).ID)
	}
	zero := int64(0)
	for _, cmd := range []Log{{Limit: MaxLogLimit}, {Since: &zero, Limit: MaxLogLimit}} {
		r := f.run(b, cmd, "")
		wantOK(t, r)
		if len(r.Data) > MaxLogPageBytes+64<<10 {
			t.Fatalf("page of %d bytes", len(r.Data))
		}
		page := decode[LogResult](t, r)
		if len(page.Messages) == 0 || len(page.Messages) == len(want) || page.Next == nil {
			t.Fatalf("%d messages, next %v", len(page.Messages), page.Next)
		}
		for _, e := range page.Messages {
			if e.Body != body {
				t.Fatal("a body was cut")
			}
		}
		if cmd.Since == nil && logIDs(page)[len(page.Messages)-1] != want[len(want)-1] {
			t.Fatal("the latest page must keep the newest message")
		}
	}
}

// A send replayed after a recipient left says the delivery waits for it to
// come back, even when another member now has its alias.
func TestReplayShowsDeliveryHeldForADepartedRecipient(t *testing.T) {
	f, a, b, _ := channel3(t)
	wantOK(t, f.run(a, Note{To: []string{"tax"}, Body: "fyi"}, "n"))
	wantOK(t, f.run(b, Leave{}, ""))
	successor := f.newSession("codex")
	wantOK(t, f.join(successor, Join{Channel: "checkout", Alias: "tax"}, ""))
	d := decode[NoteResult](t, f.run(a, Note{To: []string{"tax"}, Body: "fyi"}, "n")).Recipients
	if len(d) != 1 || d[0].Alias != "tax" || d[0].State != "stored" || d[0].Next != "held until it rejoins" {
		t.Fatalf("delivery %+v", d)
	}
}

// Leave returns, and replays, the pending IDs and roster changes it saw
// while the member was still live.
func TestLeaveKeepsPendingAndRosterChanges(t *testing.T) {
	f, a, b, _ := channel3(t)
	seq := uint64(f.count(`SELECT MAX(seq) FROM roster_change`))
	if _, err := f.db.Exec(`UPDATE binding SET hint_acked = 1, roster_ack_seq = ?`, seq); err != nil {
		t.Fatal(err)
	}
	wantOK(t, f.run(a, Focus{Text: "refunds"}, "f"))
	note := decode[NoteResult](t, f.run(a, Note{To: []string{"tax"}, Body: "fyi"}, "n")).ID
	for _, r := range []core.Result{
		f.exec(f.resolved(b), Request{OpKey: "bye", Command: Leave{}}),
		f.exec(f.resolved(b), Request{OpKey: "bye", Command: Leave{}}),
	} {
		wantOK(t, r)
		got := decode[memberOutput](t, r)
		if !reflect.DeepEqual(got.Pending, []int64{note}) || len(got.Changes) != 1 || got.Changes[0].Alias != "form" {
			t.Fatalf("leave output %+v", got)
		}
	}
}

type memberOutput struct {
	Pending []int64  `json:"pending"`
	Changes []Member `json:"roster_changes"`
}
