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

// A hook frame queued before spoke migration 006 gets the leaf generation
// its token names, so the first pass after the upgrade counts it in that
// generation's released mark.
func TestASpokeUpgradeNamesTheGenerationOfQueuedHookFrames(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	old, err := open(ctx, dir, spokeMigrations[:5])
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
