package store

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The first exchange has one open and one answered obligation. Use the
// original migration to create it so the upgrade starts from a real v1 store.
const adapterMigrationFixture = `
INSERT INTO channel(id, name, created_at_ms) VALUES (1, 'checkout', 1);
INSERT INTO principal(id, kind, vendor, root_tokens, created_at_ms) VALUES
    (1, 'session', 'route', 2, 1), (2, 'session', 'route', 0, 1), (3, 'session', 'route', 0, 1);
INSERT INTO principal_key(vendor, key, principal_id, created_at_ms) VALUES
    ('route', 'fixture-sender', 1, 1), ('route', 'fixture-reader', 2, 1), ('route', 'fixture-reviewer', 3, 1);
INSERT INTO binding(id, principal_id, channel_id, alias, joined_at_ms, roster_ack_seq, hint_acked) VALUES
    (1, 1, 1, 'author', 1, 12, 1), (2, 2, 1, 'reader', 1, 12, 1), (3, 3, 1, 'reviewer', 1, 12, 1);
INSERT INTO exchange(id, channel_id, root_message_id, opened_by_id, granted, spent) VALUES (1, 1, 1, 1, 8, 2);
INSERT INTO message(id, channel_id, sender_id, kind, exchange_id, audience_mode, due_at_ms, body, created_at_ms)
    VALUES (1, 1, 1, 'ask', 1, 'explicit', 60001, 'Check the totals', 1);
INSERT INTO message(id, channel_id, sender_id, kind, outcome, in_reply_to, exchange_id, audience_mode, body, created_at_ms)
    VALUES (2, 1, 3, 'reply', 'answer', 1, 1, 'explicit', 'The totals match', 20);
INSERT INTO recipient(message_id, principal_id, alias_at_commit, owes, picked_up_at_ms) VALUES
    (1, 2, 'reader', 1, 10), (1, 3, 'reviewer', 1, 11), (2, 1, 'author', 0, NULL);
INSERT INTO obligation(message_id, principal_id, state, settled_at_ms, settled_by_id, settlement_message_id) VALUES
    (1, 2, 'open', NULL, NULL, NULL), (1, 3, 'replied', 20, 3, 2);
INSERT INTO receipt(message_id, principal_id, stage, seq, at_ms, evidence) VALUES
    (1, 2, 'stored', 1, 1, 'dispatcher'), (1, 2, 'picked_up', 1, 10, 'cli'),
    (1, 3, 'stored', 1, 1, 'dispatcher'), (1, 3, 'picked_up', 1, 11, 'cli'),
    (1, 3, 'settled', 1, 20, 'cli'), (2, 1, 'stored', 1, 20, 'dispatcher');
INSERT INTO budget_event(exchange_id, delta, reason, message_id, by_id, at_ms) VALUES
    (1, 8, 'initial', NULL, 1, 1), (1, -2, 'debit', 1, 1, 1);
INSERT INTO observation(principal_id, kind, origin, source, observed_at_ms) VALUES (2, 'command', 'unknown', 'cli', 10);
INSERT INTO op(principal_id, op_key, scope, verb, payload_hash, result, at_ms) VALUES
    (1, 'fixture-ask', 'channel:checkout', 'ask', 'fixture-hash', '{"id":1}', 1);
`

func TestAdapterMigrationPreservesLedgerAndBackup(t *testing.T) {
	root := t.TempDir()
	ctx := t.Context()
	v1, err := openWithMigrations(ctx, root, []string{initialSchema})
	if err != nil {
		t.Fatal(err)
	}
	seedAdapterLedger(t, v1)
	before := adapterLedgerSnapshot(t, v1.db, 1)
	if err := v1.Close(); err != nil {
		t.Fatal(err)
	}

	v2, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer v2.Close()
	if after := adapterLedgerSnapshot(t, v2.db, 2); after != before {
		t.Fatalf("migration changed the ledger\nbefore: %s\nafter: %s", before, after)
	}
	var newEvidence int
	if err := v2.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM observation WHERE launch_ref IS NOT NULL OR event_ref IS NOT NULL
		    OR turn_ref IS NOT NULL OR hook_at_ms IS NOT NULL OR profile IS NOT NULL) +
		(SELECT COUNT(*) FROM binding WHERE roster_baseline_seq IS NOT NULL OR departed_by_hook <> 0) +
		(SELECT COUNT(*) FROM adapter_policy) + (SELECT COUNT(*) FROM adapter_launch) +
		(SELECT COUNT(*) FROM offer) + (SELECT COUNT(*) FROM capability_evidence) +
		(SELECT COUNT(*) FROM adapter_dirty)`).Scan(&newEvidence); err != nil || newEvidence != 0 {
		t.Fatalf("migration invented adapter state: rows=%d err=%v", newEvidence, err)
	}
	if err := v2.Close(); err != nil {
		t.Fatal(err)
	}
	backupPaths, err := filepath.Glob(filepath.Join(root, "store-v1-*.sqlite3"))
	if err != nil || len(backupPaths) != 1 {
		t.Fatalf("expected exactly one backup: %v (%v)", backupPaths, err)
	}
	info, err := os.Stat(backupPaths[0])
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode() != 0600 {
		t.Fatalf("unexpected backup mode: %v", info.Mode())
	}
	uri := &url.URL{Scheme: "file", Path: backupPaths[0], RawQuery: "mode=ro"}
	backup, err := sql.Open("sqlite", uri.String())
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	if restored := adapterLedgerSnapshot(t, backup, 1); restored != before {
		t.Fatalf("backup differs from v1 ledger\nbefore: %s\nbackup: %s", before, restored)
	}
	old, err := openWithMigrations(ctx, root, []string{initialSchema})
	if err == nil {
		old.Close()
		t.Fatal("a v1 store reader accepted schema 2")
	}
	if !strings.Contains(err.Error(), "unsupported schema version 2") {
		t.Fatalf("v1 refusal: %v", err)
	}
}

func openWithMigrations(ctx context.Context, root string, steps []string) (*Store, error) {
	saved := migrations
	migrations = steps
	defer func() { migrations = saved }()
	return Open(ctx, root)
}

func seedAdapterLedger(t *testing.T, db *Store) {
	t.Helper()
	if err := db.Update(t.Context(), func(tx Tx) error {
		_, err := tx.ExecContext(t.Context(), adapterMigrationFixture)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func adapterLedgerSnapshot(t *testing.T, db *sql.DB, expectedVersion int) string {
	t.Helper()
	ctx := t.Context()
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != expectedVersion {
		t.Fatalf("schema=%d, want %d (%v)", version, expectedVersion, err)
	}
	var snapshot string
	err := db.QueryRowContext(ctx, `SELECT json_object(
		'principals', (SELECT json_group_array(json_array(id, root_tokens) ORDER BY id) FROM principal),
		'keys', (SELECT json_group_array(json_array(vendor, key, principal_id) ORDER BY vendor, key) FROM principal_key),
		'bindings', (SELECT json_group_array(json_array(id, alias, state, roster_ack_seq, hint_acked) ORDER BY id) FROM binding),
		'messages', (SELECT json_group_array(json_array(id, sender_id, body, due_at_ms, in_reply_to) ORDER BY id) FROM message),
		'recipients', (SELECT json_group_array(json_array(message_id, principal_id, alias_at_commit, picked_up_at_ms)
		    ORDER BY message_id, principal_id) FROM recipient),
		'obligations', (SELECT json_group_array(json_array(message_id, principal_id, state, settled_at_ms, settlement_message_id)
		    ORDER BY message_id, principal_id) FROM obligation),
		'receipts', (SELECT json_group_array(json_array(message_id, principal_id, stage, at_ms, evidence)
		    ORDER BY message_id, principal_id, stage, seq) FROM receipt),
		'exchanges', (SELECT json_group_array(json_array(id, root_message_id, granted, spent) ORDER BY id) FROM exchange),
		'budget_events', (SELECT json_group_array(json_array(exchange_id, delta, reason) ORDER BY seq) FROM budget_event),
		'ops', (SELECT json_group_array(json_array(principal_id, op_key, payload_hash, result) ORDER BY principal_id, op_key) FROM op))`).Scan(&snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}
