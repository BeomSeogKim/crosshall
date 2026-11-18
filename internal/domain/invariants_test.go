package domain

import (
	"database/sql"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

func TestRandomizedDomainInvariants(t *testing.T) {
	const seed = 0xC2055
	const steps = 192
	random := rand.New(rand.NewSource(seed))
	step, action := -1, "setup"
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("seed=%d step=%d action=%s", seed, step, action)
		}
	})

	f, a, b, c := channel3(t)
	sessions := []session{a, b, c}
	aliases := []string{"form", "tax", "a11y"}
	for _, alias := range aliases {
		wantOK(t, f.owner(BudgetGrant{Channel: "checkout", Alias: alias, Roots: 2}, "roots-"+alias))
	}

	var asks []AskResult
	deadlines := map[int64]int64{}
	previous := map[invariantRecipientKey]invariantRecipient{}
	covered := map[string]bool{}
	for i := range steps {
		step = i
		f.now = f.now.Add(time.Duration(1+random.Intn(36)) * time.Second)
		actor := random.Intn(len(sessions))
		other := (actor + 1 + random.Intn(2)) % len(sessions)
		kind := random.Intn(15)
		var target AskResult
		if len(asks) == 0 {
			kind = 1
		} else {
			target = asks[random.Intn(len(asks))]
		}

		var command Command
		owner := false
		switch kind {
		case 0:
			command = Ask{To: []string{aliases[other]}, Body: "review?", Due: "1m", New: true}
		case 1:
			command = Ask{All: true, Each: true, Body: "both review?", Due: "2m", New: true}
		case 2:
			command = Ask{To: []string{aliases[other]}, Body: "one more question", Due: "1m", Re: target.ID}
		case 12, 13, 14:
			command = Ask{All: true, Each: true, Body: "one more for both", Due: "2m", Re: target.ID}
		case 3, 4:
			command = Read{IDs: []int64{target.ID}}
		case 5:
			command = Read{}
		case 6:
			command = Reply{To: target.ID, Outcome: OutcomeAnswer, Body: "done"}
		case 7:
			command = Reply{To: target.ID, Outcome: OutcomeDecline}
		case 8:
			command = Reply{To: target.ID, Outcome: OutcomeMoreTime}
		case 9:
			command = Cancel{Channel: "checkout", ID: target.ID}
			owner = true
		case 10:
			command = BudgetGrant{Channel: "checkout", ExchangeID: target.Exchange, Units: int64(1 + random.Intn(3))}
			owner = true
		case 11:
			command = Note{To: []string{aliases[other]}, Body: "FYI"}
		}

		key := fmt.Sprintf("invariant-%d", step)
		var result core.Result
		if owner {
			action = fmt.Sprintf("owner %s %+v", command.Verb(), command)
			result = f.owner(command, key)
		} else {
			action = fmt.Sprintf("@%s %s %+v", aliases[actor], command.Verb(), command)
			result = f.run(sessions[actor], command, key)
		}
		if result.Status != core.ExitOK && result.Status != core.ExitRefused {
			t.Fatalf("unexpected exit %d (%s): %s", result.Status, result.Code, result.Data)
		}
		if result.Code == core.CodeBudgetExhausted || result.Code == core.CodeInsufficientCredit {
			covered["unfunded exchange"] = true
		}
		if ask, ok := command.(Ask); ok && result.Status == core.ExitOK {
			sent := decode[AskResult](t, result)
			due, err := time.ParseDuration(ask.Due)
			if err != nil {
				t.Fatal(err)
			}
			asks = append(asks, sent)
			deadlines[sent.ID] = f.now.Add(due).UnixMilli()
		}
		if read, ok := command.(Read); ok && len(read.IDs) > 0 && result.Status == core.ExitOK {
			recipient := *f.caller(sessions[actor]).PrincipalID
			if previous[invariantRecipientKey{read.IDs[0], recipient}].pickup.Valid {
				covered["repeat read"] = true
			}
		}

		assertInvariantCredit(t, f)
		current := invariantRecipients(t, f)
		for key, before := range previous {
			after := current[key]
			if before.obligation.state.Valid && before.obligation.state.String != "open" &&
				before.obligation != after.obligation {
				t.Fatalf("terminal obligation for %+v changed from %+v to %+v", key, before.obligation, after.obligation)
			}
			if before.pickup.Valid && before.pickup != after.pickup {
				t.Fatalf("pickup for %+v changed from %v to %v", key, before.pickup, after.pickup)
			}
		}
		assertInvariantOverdue(t, f, current, deadlines, covered)
		previous = current
	}

	for _, state := range []string{"open", "replied", "declined", "cancelled", "overdue", "repeat read", "unfunded exchange"} {
		if !covered[state] {
			t.Errorf("sequence did not exercise %s", state)
		}
	}
}

func assertInvariantCredit(t *testing.T, f *fixture) {
	t.Helper()
	var exchange, spent, granted, owed int64
	err := f.db.QueryRow(`SELECT id, spent, granted, owed FROM (
		SELECT e.id, e.spent, e.granted, (
			SELECT COUNT(*) FROM message m JOIN recipient r ON r.message_id = m.id
			WHERE m.exchange_id = e.id AND m.kind = 'ask' AND r.owes = 1
		) AS owed FROM exchange e
	) WHERE spent > granted OR spent <> owed ORDER BY id LIMIT 1`).Scan(&exchange, &spent, &granted, &owed)
	if err == sql.ErrNoRows {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf("exchange %d spent %d of %d for %d owed recipients", exchange, spent, granted, owed)
}

type invariantRecipientKey struct {
	message, principal int64
}

type invariantObligation struct {
	state      sql.NullString
	settledAt  sql.NullInt64
	settledBy  sql.NullInt64
	settlement sql.NullInt64
	moreTime   sql.NullInt64
	reminded   sql.NullInt64
}

type invariantRecipient struct {
	owes       bool
	pickup     sql.NullInt64
	obligation invariantObligation
}

func invariantRecipients(t *testing.T, f *fixture) map[invariantRecipientKey]invariantRecipient {
	t.Helper()
	rows, err := f.db.Query(`SELECT r.message_id, r.principal_id, r.owes, r.picked_up_at_ms,
		o.state, o.settled_at_ms, o.settled_by_id, o.settlement_message_id, o.more_time_at_ms, o.reminded_at_ms,
		COALESCE(p.count, 0), p.at_ms
		FROM recipient r
		LEFT JOIN obligation o ON o.message_id = r.message_id AND o.principal_id = r.principal_id
		LEFT JOIN (
			SELECT message_id, principal_id, COUNT(*) AS count, MAX(at_ms) AS at_ms
			FROM receipt WHERE stage = 'picked_up' GROUP BY message_id, principal_id
		) p ON p.message_id = r.message_id AND p.principal_id = r.principal_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	recipients := map[invariantRecipientKey]invariantRecipient{}
	for rows.Next() {
		var key invariantRecipientKey
		var r invariantRecipient
		var pickupCount int
		var recordedPickup sql.NullInt64
		o := &r.obligation
		if err := rows.Scan(&key.message, &key.principal, &r.owes, &r.pickup,
			&o.state, &o.settledAt, &o.settledBy, &o.settlement, &o.moreTime, &o.reminded,
			&pickupCount, &recordedPickup); err != nil {
			t.Fatal(err)
		}
		if _, duplicate := recipients[key]; duplicate || r.owes != o.state.Valid {
			t.Fatalf("recipient %+v owes=%t, has obligation=%t, duplicate=%t", key, r.owes, o.state.Valid, duplicate)
		}
		if pickupCount > 1 || recordedPickup != r.pickup {
			t.Fatalf("recipient %+v pickup=%v, has %d pickup receipts at %v", key, r.pickup, pickupCount, recordedPickup)
		}
		recipients[key] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return recipients
}

func assertInvariantOverdue(t *testing.T, f *fixture, recipients map[invariantRecipientKey]invariantRecipient,
	deadlines map[int64]int64, covered map[string]bool) {
	t.Helper()
	result := f.owner(OwnerStatus{Channel: "checkout"}, "")
	wantOK(t, result)
	status := decode[StatusResult](t, result)
	if len(status.Channels) != 1 {
		t.Fatalf("expected one channel in owner status, got %d", len(status.Channels))
	}
	projected := map[invariantRecipientKey]bool{}
	for _, ask := range status.Channels[0].Requests {
		due, known := deadlines[ask.ID]
		if !known || ask.DueAtMS != due {
			t.Fatalf("request %d due at %d, want %d", ask.ID, ask.DueAtMS, due)
		}
		for _, row := range ask.Recipients {
			key := invariantRecipientKey{ask.ID, row.PrincipalID}
			r := recipients[key]
			if projected[key] || !r.owes || row.State != r.obligation.state.String {
				t.Fatalf("invalid ledger recipient %+v state=%q, stored %+v", key, row.State, r)
			}
			projected[key] = true
			covered[row.State] = true
			overdue := r.obligation.state.String == "open" && f.now.UnixMilli() >= due
			if (row.OverdueSinceMS != nil) != overdue || (overdue && *row.OverdueSinceMS != due) {
				t.Fatalf("request %d recipient %d overdue since %v, want present=%t with deadline %d",
					ask.ID, row.PrincipalID, row.OverdueSinceMS, overdue, due)
			}
			if overdue {
				covered["overdue"] = true
			}
		}
	}
	for key, r := range recipients {
		if r.owes && !projected[key] {
			t.Fatalf("owed recipient %+v missing from owner status", key)
		}
	}
}
