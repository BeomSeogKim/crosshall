package store

import (
	"path/filepath"
	"testing"
)

// An upgrade hands out nothing: a channel that existed before the join pool
// keeps a pool of 0, so its members hold only what the owner granted.
func TestJoinPoolMigrationGivesExistingChannelsNoPool(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	v4, err := openWithMigrations(ctx, root, migrations[:4])
	if err != nil {
		t.Fatal(err)
	}
	seedAdapterLedger(t, v4)
	before := adapterLedgerSnapshot(t, v4.db, 4)
	if err := v4.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	if after := adapterLedgerSnapshot(t, upgraded.db, 5); after != before {
		t.Fatalf("migration changed the ledger\nbefore: %s\nafter: %s", before, after)
	}
	var channels, pooled int
	if err := upgraded.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(join_pool), 0) FROM channel`).Scan(&channels, &pooled); err != nil || channels == 0 || pooled != 0 {
		t.Fatalf("channels=%d pool total=%d err=%v", channels, pooled, err)
	}
	if backups, err := filepath.Glob(filepath.Join(root, "store-v4-*.sqlite3")); err != nil || len(backups) != 1 {
		t.Fatalf("schema 4 backup: %v %v", backups, err)
	}
}
