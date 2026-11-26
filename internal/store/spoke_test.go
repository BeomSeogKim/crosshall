package store

import (
	"context"
	"testing"
)

// A spoke store has only the spoke schema, and opening it again is a
// no-op.
func TestASpokeStoreHasItsOwnSchema(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	for range 2 {
		s, err := OpenSpoke(ctx, dir)
		if err != nil {
			t.Fatal(err)
		}
		var version, hall, outbox int
		err = s.Update(ctx, func(tx Tx) error {
			if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
				return err
			}
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'hall'`).Scan(&hall); err != nil {
				return err
			}
			return tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name IN ('spoke_leaf', 'link_outbox')`).Scan(&outbox)
		})
		s.Close()
		if err != nil || version != len(spokeMigrations) || hall != 0 || outbox != 2 {
			t.Fatalf("version %d, hall tables %d, spoke tables %d, %v", version, hall, outbox, err)
		}
	}
}

// A hook frame queued before spoke migration 007 gets the leaf generation
// its token names, so the first pass after the upgrade counts it in that
// generation's released mark.
func TestASpokeUpgradeNamesTheGenerationOfQueuedHookFrames(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	old, err := open(ctx, dir, spokeMigrations[:6])
	if err != nil {
		t.Fatal(err)
	}
	err = old.Update(ctx, func(tx Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO spoke_hook_outbox (frame_id, vendor, key, leaf_seq, kind, body, at_ms)
			VALUES (1, 'codex', 'session', 12, 'fact', '{"token":{"leaf_seq":12,"leaf_generation":"g1"}}', 1)`)
		return err
	})
	old.Close()
	if err != nil {
		t.Fatal(err)
	}
	s, err := OpenSpoke(ctx, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var generation string
	if err := s.View(ctx, func(tx Tx) error {
		return tx.QueryRowContext(ctx, `SELECT generation FROM spoke_hook_outbox WHERE frame_id = 1`).Scan(&generation)
	}); err != nil || generation != "g1" {
		t.Fatalf("generation %q, %v", generation, err)
	}
}

// A fresh spoke store applies every spoke migration in order: 006 (native
// wakes) and 007 (hook order) both, and ends at the last version.
func TestAFreshSpokeStoreHasNativeWakesAndHookOrder(t *testing.T) {
	ctx := context.Background()
	s, err := OpenSpoke(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var version, routes, marks, generations int
	if err := s.View(ctx, func(tx Tx) error {
		if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'spoke_native_route'`).Scan(&routes); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('spoke_leaf') WHERE name = 'hook_sent_seq'`).Scan(&marks); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('spoke_hook_outbox') WHERE name = 'generation'`).Scan(&generations)
	}); err != nil {
		t.Fatal(err)
	}
	if version != 7 || len(spokeMigrations) != 7 || routes != 1 || marks != 1 || generations != 1 {
		t.Fatalf("version %d of %d, native routes %d, hook marks %d, generations %d", version, len(spokeMigrations), routes, marks, generations)
	}
}
