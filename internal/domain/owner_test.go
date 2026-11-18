package domain

import (
	"math"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

type ownerAskFixture struct {
	sender, first, second session
	message, exchange     int64
}

func seedOwnerAsk(t *testing.T, f *fixture) ownerAskFixture {
	t.Helper()
	wantOK(t, f.owner(CreateChannel{Name: "checkout"}, "channel"))
	wantOK(t, f.owner(CreateChannel{Name: "inventory"}, "other-channel"))
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

func TestOwnerBudgetGrantsExactTargetsAndReplays(t *testing.T) {
	f := newFixture(t)
	thread := seedOwnerAsk(t, f)
	owner := core.Caller{Role: core.RoleOwner}
	other := f.newSession("route")
	wantOK(t, f.join(other, Join{Channel: "inventory", Alias: "review"}, "join"))
	units := BudgetGrant{Channel: "checkout", ExchangeID: thread.exchange, Units: 5}
	roots := BudgetGrant{Channel: "checkout", Alias: "review", Roots: 2}
	wantCode(t, f.ownerMutation(f.caller(thread.sender), units, "no-role"), core.ExitRefused, core.CodeOwnerOnly)
	wantCode(t, f.ownerMutation(owner, BudgetGrant{Channel: "inventory", ExchangeID: thread.exchange, Units: 1}, "wrong-exchange"), core.ExitRefused, core.CodeNotFound)

	exchange := decode[BudgetGrantResult](t, f.ownerMutation(owner, units, "units"))
	if exchange != (BudgetGrantResult{Channel: "checkout", ExchangeID: thread.exchange, Units: 5, Remaining: 5}) {
		t.Fatalf("exchange grant %+v", exchange)
	}
	if replayed := decode[BudgetGrantResult](t, f.ownerMutation(owner, units, "units")); replayed != exchange {
		t.Fatalf("replayed exchange grant %+v", replayed)
	}
	wantCode(t, f.ownerMutation(owner, BudgetGrant{Channel: "checkout", ExchangeID: thread.exchange, Units: 6}, "units"), core.ExitRefused, core.CodeKeyConflict)
	if n := f.count(`SELECT count(*) FROM exchange WHERE granted = 7 AND spent = 2 AND episode_open = 0 AND episode_count = 1`); n != 1 {
		t.Fatalf("budget was not granted or episode reopened: %d", n)
	}
	person := decode[BudgetGrantResult](t, f.ownerMutation(owner, roots, "roots"))
	if person != (BudgetGrantResult{Channel: "checkout", Alias: "review", Roots: 2, RootTokens: 2}) {
		t.Fatalf("root grant %+v", person)
	}
	if n := f.count("SELECT root_tokens FROM principal WHERE id = ?", *f.caller(other).PrincipalID); n != 0 {
		t.Fatalf("other channel's alias received roots: %d", n)
	}
	if _, err := f.db.Exec(`UPDATE binding SET state = 'departed' WHERE principal_id = ?`, *f.caller(thread.first).PrincipalID); err != nil {
		t.Fatal(err)
	}
	if retry := decode[BudgetGrantResult](t, f.ownerMutation(owner, roots, "roots")); retry != person {
		t.Fatalf("replay did not precede live-target check: %+v", retry)
	}
	wantCode(t, f.ownerMutation(owner, roots, "root-after-leave"), core.ExitRefused, core.CodeNotFound)
	if n := f.count(`SELECT count(*) FROM budget_event WHERE reason IN ('grant', 'roots')`); n != 2 {
		t.Fatalf("budget events: %d", n)
	}
}

func TestOwnerBudgetRejectsOverflowWithoutWriting(t *testing.T) {
	f := newFixture(t)
	thread := seedOwnerAsk(t, f)
	owner := core.Caller{Role: core.RoleOwner}
	reviewer := *f.caller(thread.first).PrincipalID
	if _, err := f.db.Exec("UPDATE principal SET root_tokens = 1 WHERE id = ?", reviewer); err != nil {
		t.Fatal(err)
	}
	for i, cmd := range []BudgetGrant{
		{Channel: "checkout", ExchangeID: thread.exchange, Units: math.MaxInt64},
		{Channel: "checkout", Alias: "review", Roots: math.MaxInt64},
		{Channel: "checkout", ExchangeID: thread.exchange, Alias: "review", Units: 1, Roots: 1},
	} {
		if r := f.ownerMutation(owner, cmd, "overflow"); r.Status != core.ExitUsage {
			t.Fatalf("invalid grant %d was not rejected: %+v", i, r)
		}
	}
	if n := f.count("SELECT count(*) FROM budget_event"); n != 0 {
		t.Fatalf("invalid grant wrote budget events: %d", n)
	}
	if n := f.count("SELECT count(*) FROM op WHERE op_key = 'overflow'"); n != 0 {
		t.Fatalf("refusal reserved an operation key: %d", n)
	}
	if n := f.count("SELECT root_tokens FROM principal WHERE id = ?", reviewer); n != 1 {
		t.Fatalf("root tokens changed: %d", n)
	}
	if n := f.count("SELECT granted FROM exchange WHERE id = ?", thread.exchange); n != 2 {
		t.Fatalf("exchange grant changed: %d", n)
	}
}
