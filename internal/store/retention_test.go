package store

import (
	"testing"
	"time"
)

// A pass deletes in write transactions of at most the chunk size, so a
// hook or command waits for one chunk, not for the whole pass (#256).
func TestRetentionDeletesInBoundedChunks(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedAdapterLedger(t, db)
	now := time.UnixMilli(1_900_000_000_000)
	old := now.Add(-RetentionWindow - time.Hour).UnixMilli()
	if err := db.Update(ctx, func(tx Tx) error {
		for i := range 7 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO op (principal_id, op_key, scope, verb, payload_hash, result, at_ms)
				VALUES (2, ?, 'channel:dev', 'note', '', '{}', ?)`, "old-"+string(rune('a'+i)), old); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO op (principal_id, op_key, scope, verb, payload_hash, result, at_ms)
			VALUES (2, 'fresh', 'channel:dev', 'note', '', '{}', ?)`, now.UnixMilli())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var keep retentionKeep
	if err := db.View(ctx, func(tx Tx) (err error) {
		keep, err = readRetentionKeep(ctx, tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	var chunks []int64
	got, err := pruneSteps(ctx, retentionCut(now), keep, 3, func(apply func(Tx) error) error {
		return db.Update(ctx, func(tx Tx) error {
			before := countOps(t, tx)
			if err := apply(tx); err != nil {
				return err
			}
			chunks = append(chunks, int64(before-countOps(t, tx)))
			return nil
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Ops < 7 {
		t.Fatalf("deleted %d ops, want the 7 old ones", got.Ops)
	}
	for _, n := range chunks {
		if n > 3 {
			t.Fatalf("one transaction deleted %d ops, over the chunk of 3: %v", n, chunks)
		}
	}
	if err := db.View(ctx, func(tx Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM op WHERE op_key = 'fresh'`).Scan(&n); err != nil || n != 1 {
			t.Fatalf("the fresh op is gone (%d, %v)", n, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.PruneRetention(ctx, now); err != nil {
		t.Fatalf("a second pass on the store: %v", err)
	}
}

func countOps(t *testing.T, tx Tx) int {
	t.Helper()
	var n int
	if err := tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM op`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
