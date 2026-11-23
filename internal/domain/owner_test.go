package domain

import (
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

type ownerAskFixture struct {
	sender, first, second session
	message, exchange     int64
}

func seedOwnerAsk(t *testing.T, f *fixture) ownerAskFixture {
	t.Helper()
	wantOK(t, f.owner(CreateChannel{Name: "checkout", Pool: pool(0)}, "channel"))
	wantOK(t, f.owner(CreateChannel{Name: "inventory", Pool: pool(0)}, "other-channel"))
	thread := ownerAskFixture{
		sender: f.newSession("route"),
		first:  f.newSession("route"),
		second: f.newSession("route"),
	}
	for _, m := range []struct {
		session session
		alias   string
	}{{thread.sender, "sender"}, {thread.first, "review"}, {thread.second, "second"}} {
		wantOK(t, f.join(m.session, Join{Channel: "checkout", Alias: m.alias}, "join"))
	}

	var channel int64
	if err := f.db.QueryRow("SELECT id FROM channel WHERE name = 'checkout'").Scan(&channel); err != nil {
		t.Fatal(err)
	}
	sender := *f.caller(thread.sender).PrincipalID
	first := *f.caller(thread.first).PrincipalID
	second := *f.caller(thread.second).PrincipalID
	due := f.now.UnixMilli() + 60_000
	tx, err := f.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	message, err := tx.Exec(`INSERT INTO message
		(channel_id, sender_id, kind, audience_mode, due_at_ms, body, created_at_ms)
		VALUES (?, ?, 'ask', 'explicit', ?, 'review this', ?)`, channel, sender, due, f.now.UnixMilli())
	if err != nil {
		t.Fatal(err)
	}
	thread.message, err = message.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	exchange, err := tx.Exec(`INSERT INTO exchange
		(channel_id, root_message_id, opened_by_id, granted, spent, episode_open, episode_count)
		VALUES (?, ?, ?, 2, 2, 1, 1)`, channel, thread.message, sender)
	if err != nil {
		t.Fatal(err)
	}
	thread.exchange, err = exchange.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec("UPDATE message SET exchange_id = ? WHERE id = ?", thread.exchange, thread.message); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO recipient (message_id, principal_id, alias_at_commit, owes)
		VALUES (?, ?, 'review', 1), (?, ?, 'second', 1)`, thread.message, first, thread.message, second); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO obligation (message_id, principal_id, state, settled_at_ms, settled_by_id)
		VALUES (?, ?, 'open', NULL, NULL), (?, ?, 'replied', 100, ?)`,
		thread.message, first, thread.message, second, second); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO receipt (message_id, principal_id, stage, seq, at_ms, evidence)
		VALUES (?, ?, 'settled', 1, 100, 'cli')`, thread.message, second); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return thread
}

func (f *fixture) ownerMutation(caller core.Caller, cmd Command, key string) core.Result {
	f.t.Helper()
	tx, err := f.db.Begin()
	if err != nil {
		f.t.Fatal(err)
	}
	defer tx.Rollback()
	req := Request{Command: cmd, OpKey: key}
	var r core.Result
	switch c := cmd.(type) {
	case Cancel:
		r, err = f.svc.cancel(f.t.Context(), tx, caller, req, c, f.now)
	case BudgetGrant:
		r, err = f.svc.grantBudget(f.t.Context(), tx, caller, req, c, f.now)
	default:
		f.t.Fatalf("unexpected owner mutation %T", cmd)
	}
	if err != nil {
		f.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		f.t.Fatal(err)
	}
	return r
}

func TestOwnerCancelSettlesOnlyOpenRecipientsAndReplays(t *testing.T) {
	f := newFixture(t)
	thread := seedOwnerAsk(t, f)
	owner := core.Caller{Role: core.RoleOwner}
	cancel := Cancel{Channel: "checkout", ID: thread.message}
	wantCode(t, f.ownerMutation(f.caller(thread.sender), cancel, "peer-cancel"), core.ExitRefused, core.CodeOwnerOnly)
	wantCode(t, f.ownerMutation(owner, Cancel{Channel: "inventory", ID: thread.message}, "wrong-channel"), core.ExitRefused, core.CodeNotFound)
	if n := f.count("SELECT count(*) FROM obligation WHERE state = 'open'"); n != 1 {
		t.Fatalf("refusal changed open obligations: %d", n)
	}

	first := decode[CancelResult](t, f.ownerMutation(owner, cancel, "cancel"))
	if first != (CancelResult{Channel: "checkout", ID: thread.message, Cancelled: 1}) {
		t.Fatalf("cancelled %+v", first)
	}
	retry := decode[CancelResult](t, f.ownerMutation(owner, cancel, "cancel"))
	if first != retry {
		t.Fatalf("replay changed %+v to %+v", first, retry)
	}
	wantCode(t, f.ownerMutation(owner, Cancel{Channel: "checkout", ID: thread.message + 1}, "cancel"), core.ExitRefused, core.CodeKeyConflict)
	if r := decode[CancelResult](t, f.ownerMutation(owner, cancel, "later-cancel")); r.Cancelled != 0 {
		t.Fatalf("settled recipients changed after retry: %+v", r)
	}
	if n := f.count(`SELECT count(*) FROM obligation o JOIN principal p ON o.settled_by_id = p.id
		WHERE o.state = 'cancelled' AND p.kind = 'owner' AND o.settled_at_ms = ?`, f.now.UnixMilli()); n != 1 {
		t.Fatalf("owner settlements: %d", n)
	}
	if n := f.count(`SELECT count(*) FROM obligation WHERE state = 'replied' AND settled_at_ms = 100`); n != 1 {
		t.Fatalf("earlier settlement was lost: %d", n)
	}
	if n := f.count(`SELECT count(*) FROM receipt WHERE stage = 'settled' AND evidence = 'owner' AND at_ms = ?`, f.now.UnixMilli()); n != 1 {
		t.Fatalf("owner evidence rows: %d", n)
	}
	if n := f.count("SELECT count(*) FROM event WHERE kind = 'request_cancelled'"); n != 1 {
		t.Fatalf("cancellation events: %d", n)
	}
}

func TestGuardGrantsScopeTargetsAndReplay(t *testing.T) {
	f := newFixture(t)
	thread := seedOwnerAsk(t, f)
	owner := core.Caller{Role: core.RoleOwner}
	other := f.newSession("route")
	wantOK(t, f.join(other, Join{Channel: "inventory", Alias: "review"}, "join"))
	legacy := BudgetGrant{Channel: "checkout", ExchangeID: thread.exchange, Units: 5}
	member := BudgetGrant{Channel: "checkout", Alias: "review", Rounds: rounds(0)}
	wantCode(t, f.ownerMutation(f.caller(thread.sender), legacy, "no-role"), core.ExitRefused, core.CodeOwnerOnly)
	wantCode(t, f.owner(BudgetGrant{Channel: "inventory", ExchangeID: thread.exchange}, "wrong-exchange"), core.ExitRefused, core.CodeNotFound)

	r := f.owner(legacy, "thread")
	wantOK(t, r)
	exchange := decode[BudgetGrantResult](t, r)
	if exchange.Rounds == nil || *exchange.Rounds != 5 || exchange.ExchangeID != thread.exchange {
		t.Fatalf("legacy positional grant: %+v", exchange)
	}
	again := f.owner(legacy, "thread")
	wantOK(t, again)
	if string(r.Data) != string(again.Data) {
		t.Fatalf("replayed exchange grant: %s", again.Data)
	}
	wantCode(t, f.owner(BudgetGrant{Channel: "checkout", ExchangeID: thread.exchange, Units: 6}, "thread"), core.ExitRefused, core.CodeKeyConflict)
	if n := f.count(`SELECT count(*) FROM exchange WHERE granted = 2 AND spent = 2 AND episode_open = 0 AND episode_count = 1`); n != 1 {
		t.Fatalf("thread reset changed legacy accounting or missed the stop: %d", n)
	}
	r = f.owner(member, "member")
	wantOK(t, r)
	got := decode[BudgetGrantResult](t, r)
	if got.Rounds == nil || *got.Rounds != 0 || got.Alias != "review" {
		t.Fatalf("member grant: %+v", got)
	}
	if n := f.count("SELECT round_limit FROM principal WHERE id = ?", *f.caller(other).PrincipalID); n != 8 {
		t.Fatalf("other channel's alias was reset to %d", n)
	}
	cmd := Ask{To: []string{"sender"}, New: true, Body: "isolated"}
	wantCode(t, f.ask(thread.first, cmd, "ask"), core.ExitRefused, core.CodeRoundLimit)
	f.grantMember("review", 8)
	if sent := f.guardAsk(thread.first, cmd, "ask"); sent.Round != 1 {
		t.Fatalf("member grant did not clear the stop: %+v", sent)
	}
	executeGuardFixture(t, f, `UPDATE binding SET state = 'departed' WHERE principal_id = ?`, *f.caller(thread.first).PrincipalID)
	if retry := f.owner(member, "member"); string(retry.Data) != string(r.Data) {
		t.Fatalf("retry did not precede the live-target check: %s", retry.Data)
	}
	wantCode(t, f.ownerMutation(owner, member, "after-leave"), core.ExitRefused, core.CodeNotFound)
	if n := f.count(`SELECT count(*) FROM budget_event`); n != 0 {
		t.Fatalf("grants wrote legacy events: %d", n)
	}
}

func TestGuardGrantResetsBothRootsOfOneOldExchange(t *testing.T) {
	f, a, b, _ := channel3(t)
	first := f.guardAsk(a, Ask{To: []string{"tax"}, Body: "first"}, "")
	second := f.guardAsk(a, Ask{To: []string{"tax"}, Body: "second"}, "")
	executeGuardFixture(t, f, `UPDATE message SET exchange_id = ?, level_ms = ? WHERE id = ?`,
		first.Exchange, ms(f.now)+8*roundDurationMS, second.ID)
	wantOK(t, f.run(b, Read{IDs: []int64{first.ID, second.ID}}, ""))
	f.grantThread(first.Exchange, 0)
	if f.anyGuardLevel(b) != f.messageLevel(second.ID) || f.count(`SELECT COUNT(*) FROM thread_position`) != 0 {
		t.Fatal("thread grant touched the member position or missed a root")
	}
	for _, id := range []int64{first.ID, second.ID} {
		r := f.ask(b, Ask{Re: id, To: []string{"form"}, Body: "follow"}, "")
		wantCode(t, r, core.ExitRefused, core.CodeRoundLimit)
	}
	if f.count(`SELECT COUNT(*) FROM event WHERE kind = ?`, guardStopped) != 1 {
		t.Fatal("two roots of the same historical exchange recorded two stops")
	}
	f.grantThread(first.Exchange, 8)
	for _, id := range []int64{first.ID, second.ID} {
		got := f.guardAsk(b, Ask{Re: id, To: []string{"form"}, Body: "follow"}, "")
		if got.Round != 1 || got.Exchange != first.Exchange {
			t.Fatalf("root %d was not reset: %+v", id, got)
		}
	}
}

func TestRetiredBudgetFormsAndOldGrantReplay(t *testing.T) {
	f, _, _, _ := channel3(t)
	legacy := BudgetGrant{Channel: "checkout", Alias: "tax", Roots: 3}
	tx, err := f.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	actor, err := ownerPrincipal(t.Context(), tx, f.now)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := OK(map[string]any{"channel": "checkout", "alias": "tax", "roots": 3, "root_tokens": 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := record(t.Context(), tx, actor, "old", "channel:checkout", legacy, saved, f.now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := f.owner(legacy, "old"); got.Status != core.ExitOK || string(got.Data) != string(saved.Data) {
		t.Fatalf("old grant replay: %+v", got)
	}
	before := f.state()
	for _, cmd := range []Command{legacy, BudgetGrant{Channel: "checkout", Pool: 3},
		BudgetPolicy{Channel: "checkout", Refill: rounds(0)}, BudgetPolicy{Channel: "checkout", Ceiling: rounds(3)}} {
		got := f.owner(cmd, "retired")
		wantCode(t, got, core.ExitRefused, core.CodeRetired)
		if hint := decode[Refusal](t, got).Hint; !strings.Contains(hint, "--rounds") {
			t.Fatalf("retired help: %s", hint)
		}
	}
	if f.state() != before {
		t.Fatal("retired forms changed the store")
	}
	created := f.owner(CreateChannel{Name: "new", Pool: pool(9)}, "create")
	wantOK(t, created)
	if !decode[ChannelResult](t, created).PoolIgnored || f.count(`SELECT join_pool FROM channel WHERE name = 'new'`) != 0 {
		t.Fatal("channel creation applied the retired pool option")
	}
}
