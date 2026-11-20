package store

import (
	"database/sql"
	"net/url"
	"path/filepath"
	"testing"
)

func TestPublishMigrationBackfillsAnnouncementClaims(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	v7, err := openWithMigrations(ctx, root, migrations[:7])
	if err != nil {
		t.Fatal(err)
	}
	seedAdapterLedger(t, v7)
	before := adapterLedgerSnapshot(t, v7.db, 7)
	if _, err := v7.db.ExecContext(ctx, `INSERT INTO adapter_launch
		(principal_id, launch_ref, profile, verified, started_hook_at_ms) VALUES (2, 'launch', '{}', 1, 1);
		INSERT INTO turn (principal_id, launch_ref, turn_ref, opened_hook_at_ms) VALUES (2, 'launch', 'turn', 2);
		INSERT INTO turn_end_notice VALUES (15, 2, 'claude', 'launch', 'turn', 3);
		INSERT INTO turn_end_notice_item VALUES (15, 2, 1)`); err != nil {
		t.Fatal(err)
	}
	if err := v7.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := openWithMigrations(ctx, root, migrations[:8])
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	if after := adapterLedgerSnapshot(t, upgraded.db, 8); after != before {
		t.Fatalf("ledger changed from %s to %s", before, after)
	}
	var count int
	if err := upgraded.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM announcement
		WHERE principal_id = 2 AND message_id = 1 AND via = 'turn_end' AND ref = 15 AND at_ms = 3`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("backfilled claims = %d (%v)", count, err)
	}
	if _, err := upgraded.db.ExecContext(ctx, `INSERT INTO announcement VALUES (2, 1, 'publish', 16, 4)`); err == nil {
		t.Fatal("publish claimed a message already announced at turn-end")
	}
	backups, err := filepath.Glob(filepath.Join(root, "store-v7-*.sqlite3"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("pre-migration backups = %v (%v)", backups, err)
	}
	uri := &url.URL{Scheme: "file", Path: backups[0], RawQuery: "mode=ro"}
	backup, err := sql.Open("sqlite", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	if err := backup.QueryRowContext(ctx, `SELECT COUNT(*) FROM turn_end_notice_item WHERE notice_id = 15 AND principal_id = 2 AND message_id = 1`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("original claim in backup = %d (%v)", count, err)
	}
}
