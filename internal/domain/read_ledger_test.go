package domain

import (
	"database/sql"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

func ledgerExec(t *testing.T, f *fixture, query string, args ...any) sql.Result {
	t.Helper()
	r, err := f.db.Exec(query, args...)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func ledgerID(t *testing.T, result sql.Result) int64 {
	t.Helper()
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func ledgerMember(t *testing.T, f *fixture, name, alias string) session {
	t.Helper()
	s := f.newSession("route")
	wantOK(t, f.join(s, Join{Channel: name, Alias: alias}, ""))
	return s
}

func ledgerCaller(t *testing.T, f *fixture, s session) int64 {
	t.Helper()
	id := f.caller(s).PrincipalID
	if id == nil {
		t.Fatal("missing principal")
	}
	return *id
}

func seedAsk(t *testing.T, f *fixture, name, body string, sender, recipient session) (int64, int64) {
	t.Helper()
	from, to := ledgerCaller(t, f, sender), ledgerCaller(t, f, recipient)
	var channel int64
	var alias string
	if err := f.db.QueryRow("SELECT b.channel_id,b.alias FROM binding b JOIN channel c ON c.id=b.channel_id WHERE b.principal_id=? AND c.name=?", to, name).Scan(&channel, &alias); err != nil {
		t.Fatal(err)
	}
	now, due := ms(f.now), ms(f.now.Add(time.Minute))
	message := ledgerID(t, ledgerExec(t, f, `INSERT INTO message(channel_id,sender_id,kind,audience_mode,due_at_ms,body,created_at_ms)
		VALUES (?,?,'ask','explicit',?,?,?)`, channel, from, due, body, now))
	exchange := ledgerID(t, ledgerExec(t, f, `INSERT INTO exchange(channel_id,root_message_id,opened_by_id,granted,spent)
		VALUES (?,?,?,8,1)`, channel, message, from))
	ledgerExec(t, f, "UPDATE message SET exchange_id=? WHERE id=?", exchange, message)
	ledgerExec(t, f, "INSERT INTO recipient(message_id,principal_id,alias_at_commit,owes) VALUES (?,?,?,1)", message, to, alias)
	ledgerExec(t, f, "INSERT INTO obligation(message_id,principal_id) VALUES (?,?)", message, to)
	ledgerExec(t, f, "INSERT INTO receipt VALUES (?,?,'stored',1,?,'dispatcher')", message, to, now)
	return message, exchange
}

func actLedger(f *fixture, member session, cmd Command, key string) core.Result {
	return f.exec(f.caller(member), Request{Command: cmd, OpKey: key})
}

func TestReadBatchAuthorizationAndSnapshotRetry(t *testing.T) {
	f := newFixture(t)
	wantOK(t, f.owner(CreateChannel{Name: "checkout"}, ""))
	wantOK(t, f.owner(CreateChannel{Name: "outside"}, ""))
	a := ledgerMember(t, f, "checkout", "author")
	b := ledgerMember(t, f, "checkout", "reader")
	sibling := ledgerMember(t, f, "checkout", "observer")
	x := ledgerMember(t, f, "outside", "outside-a")
	y := ledgerMember(t, f, "outside", "outside-b")
	id, _ := seedAsk(t, f, "checkout", "private to the channel", a, b)
	level := ms(f.now) + roundDurationMS
	ledgerExec(t, f, "UPDATE message SET level_ms = ? WHERE id = ?", level, id)
	foreign, _ := seedAsk(t, f, "outside", "other channel", x, y)

	wantCode(t, actLedger(f, b, Read{IDs: []int64{id, foreign}}, "bad"),
		core.ExitRefused, core.CodeNotFound)
	if n := f.count("SELECT count(*) FROM receipt WHERE stage='picked_up'"); n != 0 {
		t.Fatal("refused batch recorded a pickup")
	}
	other := decode[ReadResult](t, actLedger(f, sibling, Read{IDs: []int64{id}}, "history"))
	if len(other.Messages) != 1 || other.Messages[0].Body != "private to the channel" ||
		f.count("SELECT count(*) FROM receipt WHERE stage='picked_up'") != 0 {
		t.Fatalf("shared history changed addressed pickup: %+v", other)
	}

	initial := decode[ReadResult](t, actLedger(f, b, Read{}, "replay"))
	newID, _ := seedAsk(t, f, "checkout", "a newer message", a, b)
	replayed := actLedger(f, b, Read{}, "replay")
	got := decode[ReadResult](t, replayed)
	if !reflect.DeepEqual(initial, got) || len(got.Messages) != 1 || got.Messages[0].ID != id {
		t.Fatalf("read replay changed the selection: %+v", got)
	}
	var meta struct{ Pending []int64 }
	if err := json.Unmarshal(replayed.Data, &meta); err != nil || !reflect.DeepEqual(meta.Pending, []int64{newID}) {
		t.Fatalf("read replay did not refresh pending: %s (%v)", replayed.Data, err)
	}
	if n := f.count("SELECT count(*) FROM receipt WHERE stage='picked_up'"); n != 1 {
		t.Fatalf("pickups = %d", n)
	}
	var saved string
	if err := f.db.QueryRow("SELECT result FROM op WHERE principal_id=? AND op_key='replay'", ledgerCaller(t, f, b)).Scan(&saved); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(saved, "private to the channel") || !strings.Contains(saved, "items") {
		t.Fatalf("operation log copied the body: %s", saved)
	}
	position := f.count("SELECT level_ms FROM thread_position WHERE thread_root_id = ? AND principal_id = ?", id, ledgerCaller(t, f, b))
	if int64(position) != level || f.count("SELECT COUNT(*) FROM binding WHERE context_exchange_id IS NOT NULL") != 0 {
		t.Fatalf("pickup position = %d, want %d with no legacy context", position, level)
	}
}

func TestInboxAndWaitingFollowAddressedReplies(t *testing.T) {
	f := newFixture(t)
	wantOK(t, f.owner(CreateChannel{Name: "checkout"}, ""))
	a := ledgerMember(t, f, "checkout", "author")
	b := ledgerMember(t, f, "checkout", "reader")
	other := ledgerMember(t, f, "checkout", "observer")
	ask, exchange := seedAsk(t, f, "checkout", "question", a, b)
	in := decode[InboxResult](t, actLedger(f, b, Inbox{}, ""))
	if len(in.Unread) != 1 || len(in.Owed) != 0 {
		t.Fatalf("before read: %+v", in)
	}
	wantOK(t, actLedger(f, b, Read{IDs: []int64{ask}}, "pickup"))
	in = decode[InboxResult](t, actLedger(f, b, Inbox{}, ""))
	if len(in.Unread) != 0 || len(in.Owed) != 1 {
		t.Fatalf("after read: %+v", in)
	}
	f.now = f.now.Add(2 * time.Minute)
	from, to := ledgerCaller(t, f, b), ledgerCaller(t, f, a)
	reply := ledgerID(t, ledgerExec(t, f, `INSERT INTO message(channel_id,sender_id,kind,outcome,in_reply_to,exchange_id,audience_mode,body,created_at_ms)
		SELECT channel_id,?,'reply','answer',id,?,'explicit','answer body',? FROM message WHERE id=?`, from, exchange, ms(f.now), ask))
	ledgerExec(t, f, "INSERT INTO recipient(message_id,principal_id,alias_at_commit,owes) VALUES (?,?,'author',0)", reply, to)
	ledgerExec(t, f, `UPDATE obligation SET state='replied',settled_at_ms=?,settled_by_id=?,settlement_message_id=? WHERE message_id=? AND principal_id=?`, ms(f.now), from, reply, ask, from)
	ledgerExec(t, f, "INSERT INTO receipt VALUES (?,?,'settled',1,?,'cli')", ask, from, ms(f.now))

	wait := decode[WaitingResult](t, actLedger(f, a, Waiting{}, ""))
	if len(wait.Requests) != 1 || !wait.Requests[0].Recipients[0].ReplyUnread ||
		wait.Requests[0].Recipients[0].LateByMS == nil {
		t.Fatalf("closed ask with unread reply missing: %+v", wait)
	}
	public := decode[ReadResult](t, actLedger(f, other, Read{IDs: []int64{ask}}, "history"))
	if !reflect.DeepEqual(public.Messages[0].ReplyIDs, []int64{reply}) || len(public.Messages[0].Replies) != 0 {
		t.Fatalf("peer viewing the thread received a reply body: %+v", public)
	}
	received := decode[ReadResult](t, actLedger(f, a, Read{IDs: []int64{ask}}, "thread"))
	if replies := received.Messages[0].Replies; len(replies) != 1 || replies[0].Body != "answer body" {
		t.Fatalf("asker's thread missing the addressed answer: %+v", received)
	}
	if left := decode[WaitingResult](t, actLedger(f, a, Waiting{}, "")); len(left.Requests) != 0 {
		t.Fatalf("waiting kept a settled, fully read ask: %+v", left)
	}
}

func TestMoreTimeIsWaitingStatusAndHistory(t *testing.T) {
	f, responder := adapterChannel(t)
	asker := f.newSession("claude")
	started := f.now.UnixMilli()
	f.hook(asker, start("pid-1", "startup", started))

	request := decode[AskResult](t, f.ask(asker, Ask{To: []string{"writer"}, Body: "review"}, ""))
	initial := decode[WaitSnapshot](t, f.run(asker, Wait{}, ""))
	f.now = f.now.Add(time.Second)
	delay := decode[ReplyResult](t, f.reply(responder, Reply{To: request.ID, Outcome: OutcomeMoreTime, Body: "after lunch"}, ""))

	inbox := decode[struct {
		InboxResult
		MemberInboxSummary
	}](t, f.run(asker, Inbox{}, ""))
	if len(inbox.Unread) != 0 || len(inbox.Pending) != 0 {
		t.Errorf("more-time reply entered the inbox: %+v", inbox)
	}
	for _, mode := range []Wait{{}, {WorkOnly: true}, {Any: true}} {
		mode.Existing = true
		if got := decode[WaitSnapshot](t, f.run(asker, mode, "")); len(got.Unread) != 0 {
			t.Errorf("more-time reply woke an existing wait: %+v", got)
		}
		mode.Cursor = &initial.Cursor
		if got := decode[WaitSnapshot](t, f.run(asker, mode, "")); len(got.Unread) != 0 || got.Cursor.MessageID != delay.ID {
			t.Errorf("more-time reply woke a pending wait or stalled its cursor: %+v", got)
		}
	}
	if got := f.hook(asker, prompt("pid-1", "after-delay", started+2_000)); got.PendingLine != "" {
		t.Errorf("boundary offered the more-time reply: %+v", got)
	}

	waiting := decode[WaitingResult](t, f.run(asker, Waiting{}, ""))
	if len(waiting.Requests) != 1 || waiting.Requests[0].DueAtMS != request.DueAtMS || len(waiting.Requests[0].Recipients) != 1 {
		t.Fatalf("waiting lost the open request or changed its deadline: %+v", waiting)
	}
	row := waiting.Requests[0].Recipients[0]
	if row.State != "open" || row.MoreTimeAtMS == nil || row.SettledAtMS != nil || row.ReplyUnread {
		t.Errorf("waiting lost the delay status or labelled it unread: %+v", row)
	}
	if got := decode[ReadResult](t, f.run(asker, Read{}, "")); len(got.Messages) != 0 {
		t.Errorf("bare read returned the more-time reply: %+v", got)
	}

	answer := decode[ReplyResult](t, f.reply(responder, Reply{To: request.ID, Body: "done"}, ""))
	messages := decode[ReadResult](t, f.run(asker, Read{}, "")).Messages
	if len(messages) != 1 || messages[0].ID != answer.ID {
		t.Errorf("bare read should return only the final reply: %+v", messages)
	}
	if got := decode[WaitingResult](t, f.run(asker, Waiting{}, "")); len(got.Requests) != 0 {
		t.Errorf("unread delay kept a finished item in waiting: %+v", got)
	}
	if n := f.count(`SELECT COUNT(*) FROM receipt WHERE message_id = ? AND stage = 'picked_up'`, delay.ID); n != 0 {
		t.Errorf("more-time reply was marked picked up without being read: %d", n)
	}

	history := decode[LogResult](t, f.run(asker, Log{}, "")).Messages
	if len(history) != 3 || history[1].ID != delay.ID || history[1].Outcome != OutcomeMoreTime || history[1].Body != "after lunch" {
		t.Errorf("history lost the more-time reply: %+v", history)
	}
	explicit := decode[ReadResult](t, f.run(asker, Read{IDs: []int64{delay.ID}}, "")).Messages
	if len(explicit) != 1 || explicit[0].Outcome != OutcomeMoreTime || explicit[0].Body != "after lunch" {
		t.Errorf("explicit read lost the more-time reply: %+v", explicit)
	}
	thread := decode[ReadResult](t, f.run(asker, Read{IDs: []int64{request.ID}}, "")).Messages
	if len(thread) != 1 || len(thread[0].Replies) != 2 || thread[0].Replies[0].ID != delay.ID {
		t.Errorf("request history lost the more-time reply: %+v", thread)
	}
}

func TestOpenRequestIsOnTimeAtItsDeadline(t *testing.T) {
	f, sender, recipient, _ := channel3(t)

	request := decode[AskResult](t, f.ask(sender, Ask{To: []string{"tax"}, Body: "time check"}, ""))
	f.now = time.UnixMilli(request.DueAtMS)

	status := func() LedgerRecipient {
		result := decode[StatusResult](t, f.owner(OwnerStatus{Channel: "checkout"}, ""))
		return result.Channels[0].Requests[0].Recipients[0]
	}
	if open := status(); open.OverdueSinceMS != nil {
		t.Fatalf("open request at its deadline is already marked overdue: %+v", open)
	}
	wantOK(t, f.reply(recipient, Reply{To: request.ID, Body: "finished"}, ""))
	settled := status()
	if settled.SettledAtMS == nil || *settled.SettledAtMS != request.DueAtMS || settled.LateByMS != nil {
		t.Fatalf("reply at the deadline is late: %+v", settled)
	}
}

func TestOwnerStatusProjectsDurableLedgerAndActiveStop(t *testing.T) {
	f := newFixture(t)
	wantOK(t, f.owner(CreateChannel{Name: "checkout"}, ""))
	wantOK(t, f.owner(CreateChannel{Name: "other"}, ""))
	a := ledgerMember(t, f, "checkout", "author")
	b := ledgerMember(t, f, "checkout", "reader")
	ask, exchange := seedAsk(t, f, "checkout", "question", a, b)
	due := ms(f.now.Add(time.Minute))
	wantOK(t, actLedger(f, b, Read{IDs: []int64{ask}}, "pickup"))
	f.now = f.now.Add(2 * time.Minute)
	ledgerExec(t, f, "UPDATE message SET level_ms = ? WHERE id = ?", ms(f.now)+8*roundDurationMS, ask)
	wantCode(t, f.ask(a, Ask{Re: ask, To: []string{"reader"}, Body: "stopped"}, ""), core.ExitRefused, core.CodeRoundLimit)
	first := decode[StatusResult](t, f.owner(OwnerStatus{}, ""))
	if len(first.Channels) != 2 || len(first.Channels[0].Bindings) != 2 || len(first.Channels[0].Requests) != 1 {
		t.Fatalf("missing ledger rows: %+v", first)
	}
	channel := first.Channels[0]
	r := channel.Requests[0].Recipients[0]
	if r.State != "open" || r.BindingID == nil || r.OverdueSinceMS == nil || *r.OverdueSinceMS != due ||
		len(r.Receipts) != 2 || r.Receipts[0].AtMS > r.Receipts[1].AtMS ||
		len(channel.Exchanges) != 1 || channel.Exchanges[0].ID != exchange || channel.Exchanges[0].Round != 8 ||
		len(channel.GuardStops) != 1 || channel.GuardStops[0].RoundLimit != 8 {
		t.Fatalf("incorrect ledger projection: %+v", channel)
	}
	f.now = f.now.Add(time.Second)
	second := decode[StatusResult](t, f.owner(OwnerStatus{}, ""))
	if first.GeneratedAtMS == second.GeneratedAtMS || !reflect.DeepEqual(first.Channels, second.Channels) {
		t.Fatalf("ledger changed with wall time after deadline: %+v vs %+v", first, second)
	}
	member := decode[StatusResult](t, actLedger(f, a, Status{}, ""))
	if len(member.Channels) != 1 || len(member.Channels[0].Bindings) != 0 ||
		len(member.Channels[0].GuardStops) != 0 || member.Channels[0].Requests[0].Recipients[0].BindingID != nil {
		t.Fatalf("member status exposed owner data: %+v", member)
	}
	wantCode(t, actLedger(f, a, OwnerStatus{}, ""), core.ExitRefused, core.CodeOwnerOnly)
}
