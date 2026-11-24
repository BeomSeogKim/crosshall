package store

import (
	"database/sql"
	"net/url"
	"path/filepath"
	"testing"
)

func TestOfferReceiptUpgradeRetriesHistoricalUnreadWork(t *testing.T) {
	ctx, root := t.Context(), t.TempDir()
	const previous = 26
	db, err := openWithMigrations(ctx, root, migrations[:previous])
	if err != nil {
		t.Fatal(err)
	}
	seedAdapterLedger(t, db)
	_, err = db.db.ExecContext(ctx, `
		INSERT INTO offer(id, principal_id, binding_id, generation, epoch, reason, evidence, at_ms)
		  VALUES (1,1,1,'gen-a',0,'prompt','fixture',23), (2,2,2,'gen-b',0,'prompt','fixture',24);
		INSERT INTO offer_item(offer_id,message_id,principal_id) VALUES (1,2,1),(2,1,2);
		DELETE FROM adapter_dirty;
		INSERT INTO adapter_dirty(principal_id, reminder) VALUES(1,1)`)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	up, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer up.Close()
	var historical, dirty, other int
	err = up.db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM offer WHERE receipt_mode='historical' AND written_at_ms IS NULL AND nonce IS NULL),
		(SELECT count(*) FROM adapter_dirty WHERE principal_id=1 AND notice=1 AND reminder=1),
		(SELECT count(*) FROM adapter_dirty WHERE principal_id<>1)`).Scan(&historical, &dirty, &other)
	if err != nil || historical != 2 || dirty != 1 || other != 0 {
		t.Fatalf("historical %d, unread dirty %d, other dirty %d: %v", historical, dirty, other, err)
	}
	backups, err := filepath.Glob(filepath.Join(root, "store-v26-*.sqlite3"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("pre-upgrade backups %v: %v", backups, err)
	}
	uri := &url.URL{Scheme: "file", Path: backups[0], RawQuery: "mode=ro"}
	backup, err := sql.Open("sqlite", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	var version, offers int
	if err := backup.QueryRowContext(ctx, `SELECT user_version,(SELECT count(*) FROM offer_item) FROM pragma_user_version`).Scan(&version, &offers); err != nil || version != previous || offers != 2 {
		t.Fatalf("backup version %d, offer items %d: %v", version, offers, err)
	}
}
