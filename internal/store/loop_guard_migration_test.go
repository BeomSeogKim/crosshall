package store

import "testing"

func TestLoopGuardMigrationPreservesLedgerAndInitializesNoPositions(t *testing.T) {
	ctx, root := t.Context(), t.TempDir()
	old, err := openWithMigrations(ctx, root, migrations[:20])
	if err != nil {
		t.Fatal(err)
	}
	seedAdapterLedger(t, old)
	before := adapterLedgerSnapshot(t, old.db, 20)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	if after := adapterLedgerSnapshot(t, upgraded.db, 21); after != before {
		t.Fatalf("migration changed the ledger\nbefore: %s\nafter: %s", before, after)
	}
	var n int
	if err := upgraded.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM message WHERE level_ms <> 0) +
		(SELECT COUNT(*) FROM principal WHERE any_level_ms <> 0 OR reset_after_message_id <> 0 OR round_limit <> 8 OR guard_told <> 0) +
		(SELECT COUNT(*) FROM exchange WHERE reset_after_message_id <> 0 OR round_limit <> 8) +
		(SELECT COUNT(*) FROM thread_position)`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("migration invented guard positions: rows=%d err=%v", n, err)
	}
}
