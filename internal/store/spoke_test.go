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
