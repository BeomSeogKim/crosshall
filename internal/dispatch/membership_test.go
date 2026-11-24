package dispatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func TestQueuedMemberCommandCannotCrossChannelMoves(t *testing.T) {
	s := reviewServer(t)
	s.fixtureSQL(t, `INSERT INTO channel (id, name, created_at_ms) VALUES (1, 'alpha', 1), (2, 'beta', 1);
		INSERT INTO principal (id, kind, vendor, created_at_ms) VALUES (1, 'session', 'route', 1);
		INSERT INTO binding (id, principal_id, channel_id, alias, joined_at_ms) VALUES (11, 1, 1, 'reader', 1);
		UPDATE binding SET state = 'departed' WHERE id = 11;
		INSERT INTO binding (id, principal_id, channel_id, alias, joined_at_ms) VALUES (22, 1, 2, 'reader', 2);`)
	const principal = int64(1)
	s.cfg.Resolve = func(ctx context.Context, tx store.Tx, _ Arrival, _ wire.Request) (Resolution, error) {
		selected, err := ident.SelectedMembership(ctx, tx, principal)
		caller := core.Caller{Role: core.RoleMember, BindingID: &selected.BindingID, Membership: &selected}
		return Resolution{Caller: caller}, err
	}
	s.cfg.Decode = func(req wire.Request, _ Resolution) (domain.Request, error) {
		return domain.Request{OpKey: req.OpKey, Command: domain.Focus{Text: "written"}}, nil
	}
	var invoked []int64
	s.cfg.Handle = handlerFunc(func(_ context.Context, _ store.Tx, caller core.Caller, _ domain.Request) (core.Result, error) {
		invoked = append(invoked, *caller.BindingID)
		return domain.OK(struct{}{})
	})
	oldA := core.Membership{BindingID: 11, Generation: 1}
	B := core.Membership{BindingID: 22, Generation: 2}
	request := func(membership *core.Membership) core.Result {
		return s.fixtureRequest(t, Arrival{}, wire.Request{Verb: domain.VerbFocus, OpKey: "focus", Membership: membership, Ack: 999})
	}
	for _, scope := range []*core.Membership{&oldA, nil} {
		result := request(scope)
		if result.Status != core.ExitRefused || result.Code != core.CodeBindingChanged || len(invoked) != 0 {
			t.Fatalf("request for %+v crossed into B: %+v, invoked %v", scope, result, invoked)
		}
	}
	if result := request(&B); result.Status != core.ExitOK || len(invoked) != 1 || invoked[0] != 22 {
		t.Fatalf("current B request: %+v, invoked %v", result, invoked)
	}
	s.fixtureSQL(t, `UPDATE binding SET state = 'departed' WHERE id = 22;
		UPDATE binding SET state = 'live' WHERE id = 11;`)
	if result := request(&oldA); result.Status != core.ExitRefused || result.Code != core.CodeBindingChanged || len(invoked) != 1 {
		t.Fatalf("old A request crossed into reactivated A: %+v, invoked %v", result, invoked)
	}
}

func TestReachReceiptFromEarlierVisitCannotAcknowledgeReactivatedBinding(t *testing.T) {
	s, root := adapterFilesFixture(t)
	leaf := adapterLeafFixture(t, s, root, true)
	s.fixtureSQL(t, `INSERT INTO channel (id, name, created_at_ms) VALUES (1, 'alpha', 1), (2, 'beta', 1);
		INSERT INTO binding (id, principal_id, channel_id, alias, joined_at_ms) VALUES (11, 1, 1, 'reader', 1);
		UPDATE binding SET state = 'departed' WHERE id = 11;
		INSERT INTO binding (id, principal_id, channel_id, alias, joined_at_ms) VALUES (22, 1, 2, 'reader', 2);
		UPDATE binding SET state = 'departed' WHERE id = 22;
		UPDATE binding SET state = 'live' WHERE id = 11;`)
	const principal = int64(1)
	writeReceipt := func(generation int64) string {
		t.Helper()
		receipt := wire.HookReachAck{Generation: "test-gen", BindingID: 11, RouteEpoch: 1, MembershipGeneration: generation, Told: 1}
		if err := publishReachOffer(leaf, &receipt); err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(receipt)
		if err != nil {
			t.Fatal(err)
		}
		if err := leaf.WriteFile("seen-reach", data, 0600); err != nil {
			t.Fatal(err)
		}
		return receipt.Offer
	}
	check := func(want sql.NullInt64) {
		t.Helper()
		if err := s.db.Update(t.Context(), func(tx store.Tx) error {
			selected, err := ident.SelectedMembership(t.Context(), tx, principal)
			if err != nil {
				return err
			}
			principalID := principal
			caller := core.Caller{Role: core.RoleMember, PrincipalID: &principalID, BindingID: &selected.BindingID, Membership: &selected}
			if err := acknowledgeHookReach(t.Context(), tx, Arrival{Dir: leaf}, wire.Request{Generation: "test-gen"}, caller); err != nil {
				return err
			}
			var got sql.NullInt64
			if err := tx.QueryRowContext(t.Context(), `SELECT reach_told FROM binding WHERE id = 11`).Scan(&got); err != nil {
				return err
			}
			if got != want {
				t.Fatalf("reach receipt applied %+v, want %+v", got, want)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	old := writeReceipt(1)
	check(sql.NullInt64{})
	if current := writeReceipt(3); current == old {
		t.Fatal("the reactivated binding reused an old offer")
	}
	check(sql.NullInt64{Int64: 1, Valid: true})
}

func TestLegacyJoinCannotMakeTheFirstChannelSwitch(t *testing.T) {
	s, root := adapterFilesFixture(t)
	leaf := adapterLeafFixture(t, s, root, true)
	s.fixtureSQL(t, `INSERT INTO channel (id, name, created_at_ms) VALUES (1, 'alpha', 1), (2, 'beta', 1);
		INSERT INTO binding (id, principal_id, channel_id, alias, joined_at_ms) VALUES (11, 1, 1, 'reader', 1);`)
	principal := int64(1)
	initial := core.Membership{BindingID: 11, Generation: 1}
	s.cfg.Resolve = func(context.Context, store.Tx, Arrival, wire.Request) (Resolution, error) {
		return Resolution{Caller: core.Caller{Role: core.RoleMember, PrincipalID: &principal,
			BindingID: &initial.BindingID, Membership: &initial}}, nil
	}
	s.cfg.Decode = func(req wire.Request, _ Resolution) (domain.Request, error) {
		return domain.Request{OpKey: req.OpKey, Command: domain.Join{Channel: "beta", Move: true}}, nil
	}
	s.cfg.Handle = handlerFunc(func(ctx context.Context, tx store.Tx, _ core.Caller, _ domain.Request) (core.Result, error) {
		if _, err := tx.ExecContext(ctx, `UPDATE binding SET state = 'departed' WHERE id = 11;
			INSERT INTO binding (id, principal_id, channel_id, alias, joined_at_ms) VALUES (22, 1, 2, 'reader', 2);`); err != nil {
			return core.Result{}, err
		}
		return domain.OK(domain.JoinResult{Channel: "beta"})
	})
	request := wire.Request{Verb: domain.VerbJoin, OpKey: "switch", Generation: "test-gen", KeyDigest: (ident.Identity{Vendor: "codex", Key: "synthetic-key"}).Digest()}
	out := s.fixtureRequest(t, Arrival{Dir: leaf}, request)
	if out.Status != core.ExitRefused || out.Code != core.CodeBindingChanged {
		t.Fatalf("legacy switch: %+v", out)
	}
	if err := s.db.Update(t.Context(), func(tx store.Tx) error {
		got, err := ident.SelectedMembership(t.Context(), tx, principal)
		if err == nil && got != initial {
			t.Fatalf("legacy refusal committed %+v", got)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	request.Membership = &initial
	if out := s.fixtureRequest(t, Arrival{Dir: leaf}, request); out.Status != core.ExitOK {
		t.Fatalf("scoped switch: %+v", out)
	}
	if got, err := ident.ReadMembership(leaf); err != nil || got != (core.Membership{BindingID: 22, Generation: 2}) {
		t.Fatalf("published scope %+v, err %v", got, err)
	}
}
