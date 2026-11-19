package store

import "testing"

func TestTurnEndMigrationPreservesEvidence(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	v3, err := openWithMigrations(ctx, root, []string{initialSchema, adapterSchema, ownerDepartSchema})
	if err != nil {
		t.Fatal(err)
	}
	seedAdapterLedger(t, v3)
	before := adapterLedgerSnapshot(t, v3.db, 3)
	if _, err := v3.db.ExecContext(ctx, `INSERT INTO capability_evidence VALUES
		('claude', '2.1.327', 'darwin-arm64', 'print', 'claude-1', 'enroll', 'tested', 'seed-enroll', 123),
		('codex', 'unverified', 'linux-amd64', 'unverified', 'codex-1', 'reminder', 'unavailable', 'seed-reminder', 456)`); err != nil {
		t.Fatal(err)
	}
	if err := v3.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	if after := adapterLedgerSnapshot(t, upgraded.db, 4); after != before {
		t.Fatalf("migration changed the ledger\nbefore: %s\nafter: %s", before, after)
	}
	var count int
	if err := upgraded.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM capability_evidence
		WHERE (vendor, vendor_version, platform, mode, manifest_rev, cap, level, result_ref, checked_at_ms) IN (
			('claude', '2.1.327', 'darwin-arm64', 'print', 'claude-1', 'enroll', 'tested', 'seed-enroll', 123),
			('codex', 'unverified', 'linux-amd64', 'unverified', 'codex-1', 'reminder', 'unavailable', 'seed-reminder', 456))`).Scan(&count); err != nil || count != 2 {
		t.Fatalf("evidence after migration = %d rows, error = %v", count, err)
	}
	if _, err := upgraded.db.ExecContext(ctx, `INSERT INTO capability_evidence VALUES
		('claude', '2.1.327', 'darwin-arm64', 'print', 'claude-1', 'turn_end', 'tested', 'new-result', 789)`); err != nil {
		t.Fatal(err)
	}
	if _, err := upgraded.db.ExecContext(ctx, `INSERT INTO capability_evidence VALUES
		('claude', '2.1.327', 'darwin-arm64', 'print', 'claude-1', 'wake', 'tested', 'unknown-cap', 789)`); err == nil {
		t.Fatal("unexpected capability passed the rebuilt CHECK")
	}
}
