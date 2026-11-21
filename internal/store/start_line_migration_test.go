package store

import (
	"database/sql"
	"errors"
	"testing"
)

// The version before the start line gave a hook-joined member its rules in
// the hook's welcome and kept no record of an "off" that changed nothing.
// The upgrade keeps both owed: the rules for a member whose welcome never
// arrived, and the owner's off.
func TestStartLineMigrationKeepsOwedRulesAndTheOwnersOff(t *testing.T) {
	ctx := t.Context()
	const offCommand = `INSERT INTO op (principal_id, op_key, scope, verb, payload_hash, result, at_ms)
		VALUES (1, 'off-1', 'channel:auto', 'owner channel auto', 'h', '{}', 77)`
	upgrade := func(t *testing.T, older string) *Store {
		t.Helper()
		root := t.TempDir()
		v4, err := openWithMigrations(ctx, root, migrations[:4])
		if err != nil {
			t.Fatal(err)
		}
		seedAdapterLedger(t, v4)
		if err := v4.Close(); err != nil {
			t.Fatal(err)
		}
		v12, err := openWithMigrations(ctx, root, migrations[:12])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := v12.db.ExecContext(ctx, older); err != nil {
			t.Fatal(err)
		}
		if err := v12.Close(); err != nil {
			t.Fatal(err)
		}
		upgraded, err := Open(ctx, root)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { upgraded.Close() })
		return upgraded
	}
	autoJoin := func(t *testing.T, s *Store) (enabled, at int64, set bool) {
		t.Helper()
		err := s.db.QueryRowContext(ctx, `SELECT enabled, changed_at_ms FROM hall_auto_join`).Scan(&enabled, &at)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
		return enabled, at, err == nil
	}

	t.Run("a welcome that never arrived", func(t *testing.T) {
		// Binding 1: a command was acknowledged while the welcome was pending.
		// Binding 2 joined by hand and has its rules.
		s := upgrade(t, `UPDATE binding SET roster_baseline_seq = 3 WHERE id = 1`)
		var pending, byHand bool
		if err := s.db.QueryRowContext(ctx, `SELECT (SELECT hint_acked = 0 AND start_line_seen = 0 FROM binding WHERE id = 1),
			(SELECT hint_acked = 1 FROM binding WHERE id = 2)`).Scan(&pending, &byHand); err != nil || !pending || !byHand {
			t.Fatalf("rules still owed %t, hand join untouched %t (%v)", pending, byHand, err)
		}
		if _, _, set := autoJoin(t, s); set {
			t.Fatal("a hall with no switch command got a choice on record")
		}
	})
	t.Run("an off that changed nothing", func(t *testing.T) {
		s := upgrade(t, offCommand)
		if enabled, at, set := autoJoin(t, s); !set || enabled != 0 || at != 77 {
			t.Fatalf("the owner's off: set %t enabled %d at %d", set, enabled, at)
		}
	})
	t.Run("a later on stays", func(t *testing.T) {
		s := upgrade(t, offCommand+`; INSERT INTO hall_auto_join (id, enabled, changed_at_ms) VALUES (1, 1, 90)`)
		if enabled, at, set := autoJoin(t, s); !set || enabled != 1 || at != 90 {
			t.Fatalf("the owner's on: set %t enabled %d at %d", set, enabled, at)
		}
	})
}
