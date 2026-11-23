package dispatch

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func TestReachReceiptStoresOnlyTheLineOfferedLast(t *testing.T) {
	s, root := adapterFilesFixture(t)
	leaf := adapterLeafFixture(t, s, root, true)
	s.fixtureSQL(t, `INSERT INTO channel (id, name, state, created_at_ms) VALUES (1, 'dev', 'active', 1);
		INSERT INTO binding (id, principal_id, channel_id, alias, joined_at_ms) VALUES (2, 1, 1, 'fixture', 1);`)
	principal, binding := int64(1), int64(2)
	member := core.Caller{Role: core.RoleMember, PrincipalID: &principal, BindingID: &binding}
	offer := func(told int) wire.HookReachAck {
		t.Helper()
		ack := wire.HookReachAck{Generation: "test-gen", BindingID: 2, RouteEpoch: 1, Told: told}
		if err := publishReachOffer(leaf, &ack); err != nil {
			t.Fatal(err)
		}
		return ack
	}
	check := func(seen wire.HookReachAck, want sql.NullInt64) {
		t.Helper()
		data, err := json.Marshal(seen)
		if err != nil {
			t.Fatal(err)
		}
		if err := leaf.WriteFile("seen-reach", data, 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.db.Update(t.Context(), func(tx store.Tx) error {
			if err := acknowledgeHookReach(t.Context(), tx, Arrival{Dir: leaf}, wire.Request{Generation: "test-gen"}, member); err != nil {
				return err
			}
			var told sql.NullInt64
			if err := tx.QueryRowContext(t.Context(), "SELECT reach_told FROM binding WHERE id = 2").Scan(&told); err != nil {
				return err
			}
			if told != want {
				t.Fatalf("receipt %+v stored %v, want %v", seen, told, want)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	nothing, brings, lost := sql.NullInt64{}, sql.NullInt64{Int64: 1, Valid: true}, sql.NullInt64{Valid: true}

	first := offer(1)
	if again := offer(1); again != first {
		t.Fatalf("an unchanged line changed its offer: %+v, %+v", again, first)
	}
	stale := first
	stale.Generation = "old-gen"
	check(stale, nothing)
	stale = first
	stale.RouteEpoch = 2
	check(stale, nothing)
	check(first, brings)

	// A later line replaces the offer: a lost print of it keeps what was told.
	second := offer(0)
	if second.Offer == first.Offer {
		t.Fatal("a changed line kept its offer")
	}
	check(first, brings)
	check(second, lost)
}

func TestRecoveryHintReceiptIsScopedToTheOfferedSuspension(t *testing.T) {
	s, root := adapterFilesFixture(t)
	leaf := adapterLeafFixture(t, s, root, true)
	s.fixtureSQL(t, `INSERT INTO channel (id, name, state, created_at_ms) VALUES (1, 'dev', 'active', 1);
		INSERT INTO binding (id, principal_id, channel_id, alias, joined_at_ms, reach_told) VALUES (2, 1, 1, 'fixture', 1, 0);
		INSERT INTO publish_suspension (id, binding_id, launch_ref, reason, notice, at_ms) VALUES
			(3, 2, 'old', 'absorbed', 3, 3), (4, 2, 'current', 'absorbed', 4, 4);`)
	principal, binding := int64(1), int64(2)
	member := core.Caller{Role: core.RoleMember, PrincipalID: &principal, BindingID: &binding}
	previous := wire.HookReachAck{Generation: "test-gen", BindingID: 2, RouteEpoch: 1, Told: 0, SuspensionID: 3}
	if err := publishReachOffer(leaf, &previous); err != nil {
		t.Fatal(err)
	}
	current := previous
	current.SuspensionID = 4
	if err := publishReachOffer(leaf, &current); err != nil {
		t.Fatal(err)
	}
	if current.Offer == previous.Offer {
		t.Fatal("the new suspension reused the older offer")
	}
	for _, receipt := range []struct {
		seen wire.HookReachAck
		want int
	}{{previous, store.RecoveryHintPending}, {current, store.RecoveryHintAcknowledged}} {
		data, err := json.Marshal(receipt.seen)
		if err != nil {
			t.Fatal(err)
		}
		if err := leaf.WriteFile("seen-reach", data, 0600); err != nil {
			t.Fatal(err)
		}
		if err := s.db.Update(t.Context(), func(tx store.Tx) error {
			if err := acknowledgeHookReach(t.Context(), tx, Arrival{Dir: leaf}, wire.Request{Generation: "test-gen"}, member); err != nil {
				return err
			}
			var oldHint, currentHint int
			if err := tx.QueryRowContext(t.Context(), `SELECT old.recovery_hint, current.recovery_hint
				FROM publish_suspension old, publish_suspension current WHERE old.id = 3 AND current.id = 4`).Scan(&oldHint, &currentHint); err != nil {
				return err
			}
			if oldHint != store.RecoveryHintPending || currentHint != receipt.want {
				t.Fatalf("receipt for suspension %d stored states %d, %d; want 0, %d", receipt.seen.SuspensionID, oldHint, currentHint, receipt.want)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
