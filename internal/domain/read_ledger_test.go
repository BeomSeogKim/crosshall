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
	id, exchange := seedAsk(t, f, "checkout", "private to the channel", a, b)
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
	gotExchange := f.count("SELECT context_exchange_id FROM binding WHERE principal_id=?", ledgerCaller(t, f, b))
	if int64(gotExchange) != exchange {
		t.Fatalf("pickup context = %d, want %d", gotExchange, exchange)
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

func TestOwnerStatusProjectsDurableLedgerAndEpisodeOrder(t *testing.T) {
	f := newFixture(t)
	wantOK(t, f.owner(CreateChannel{Name: "checkout"}, ""))
	wantOK(t, f.owner(CreateChannel{Name: "other"}, ""))
	a := ledgerMember(t, f, "checkout", "author")
	b := ledgerMember(t, f, "checkout", "reader")
	ask, exchange := seedAsk(t, f, "checkout", "question", a, b)
	due := ms(f.now.Add(time.Minute))
	wantOK(t, actLedger(f, b, Read{IDs: []int64{ask}}, "pickup"))
	f.now = f.now.Add(2 * time.Minute)
	ledgerExec(t, f, "UPDATE exchange SET spent=granted,episode_open=1,episode_count=2 WHERE id=?", exchange)
	ledgerExec(t, f, `INSERT INTO event(kind,subject,actor_id,detail,at_ms) VALUES
		('budget_exhausted','exchange:' || ?,?, '{"needed":1}',?),
		('budget_exhausted','exchange:' || ?,?, '{"needed":2}',?)`,
		exchange, ledgerCaller(t, f, a), ms(f.now)-1, exchange, ledgerCaller(t, f, a), ms(f.now))
	first := decode[StatusResult](t, f.owner(OwnerStatus{}, ""))
	if len(first.Channels) != 2 || len(first.Channels[0].Bindings) != 2 || len(first.Channels[0].Requests) != 1 {
		t.Fatalf("missing ledger rows: %+v", first)
	}
	channel := first.Channels[0]
	r := channel.Requests[0].Recipients[0]
	if r.State != "open" || r.BindingID == nil || r.OverdueSinceMS == nil || *r.OverdueSinceMS != due ||
		len(r.Receipts) != 2 || r.Receipts[0].AtMS > r.Receipts[1].AtMS ||
		channel.Exchanges[0].Spent != 8 || channel.Exchanges[0].Remaining != 0 ||
		len(channel.BudgetExhaustions) != 2 || channel.BudgetExhaustions[1].Episode != 2 {
		t.Fatalf("incorrect ledger projection: %+v", channel)
	}
	f.now = f.now.Add(time.Second)
	second := decode[StatusResult](t, f.owner(OwnerStatus{}, ""))
	if first.GeneratedAtMS == second.GeneratedAtMS || !reflect.DeepEqual(first.Channels, second.Channels) {
		t.Fatalf("ledger changed with wall time after deadline: %+v vs %+v", first, second)
	}
	member := decode[StatusResult](t, actLedger(f, a, Status{}, ""))
	if len(member.Channels) != 1 || len(member.Channels[0].Bindings) != 0 ||
		len(member.Channels[0].BudgetExhaustions) != 0 || member.Channels[0].Requests[0].Recipients[0].BindingID != nil {
		t.Fatalf("member status exposed owner data: %+v", member)
	}
	wantCode(t, actLedger(f, a, OwnerStatus{}, ""), core.ExitRefused, core.CodeOwnerOnly)
}
