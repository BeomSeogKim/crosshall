package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSessionMoveMigrationUsesSlot027(t *testing.T) {
	if len(migrations) < 27 || migrations[23] != ownerAlertSchema || migrations[24] != threadLanesSchema ||
		migrations[25] != unknownReceiptCauseSchema || migrations[26] != sessionMoveSchema {
		t.Fatal("migration 027 must follow owner alerts, per-thread publish lanes and unknown receipt causes")
	}
}

func TestSessionMoveMigrationPreservesBindingsAndLedger(t *testing.T) {
	ctx, root := t.Context(), t.TempDir()
	old, err := openWithMigrations(ctx, root, migrations[:22])
	if err != nil {
		t.Fatal(err)
	}
	seedAdapterLedger(t, old)
	if _, err := old.db.ExecContext(ctx, `UPDATE binding SET state = 'departed' WHERE id = 2;
		INSERT INTO principal (id, kind, vendor, created_at_ms) VALUES (4, 'session', 'route', 1);`); err != nil {
		t.Fatal(err)
	}
	before := adapterLedgerSnapshot(t, old.db, 22)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	if after := adapterLedgerSnapshot(t, upgraded.db, len(migrations)); before != after {
		t.Fatal("migration changed existing ledger rows")
	}
	var count int
	if err := upgraded.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM member_selection WHERE principal_id = binding_id AND generation = 1`).Scan(&count); err != nil || count != 3 {
		t.Fatalf("selected %d original bindings, want 3: %v", count, err)
	}
	var selectedCount int
	if err := upgraded.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM member_selection`).Scan(&selectedCount); err != nil || selectedCount != count {
		t.Fatalf("new selection for a never-enrolled session: %d, want %d: %v", selectedCount, count, err)
	}
}

func TestSessionMoveMigrationRefusesAmbiguousBindingsAndKeepsRollback(t *testing.T) {
	ctx, root := t.Context(), t.TempDir()
	old, err := openWithMigrations(ctx, root, migrations[:22])
	if err != nil {
		t.Fatal(err)
	}
	seedAdapterLedger(t, old)
	if _, err := old.db.ExecContext(ctx, `INSERT INTO channel (id, name, created_at_ms) VALUES (2, 'side', 5);
		INSERT INTO binding (id, principal_id, channel_id, alias, state, joined_at_ms) VALUES (4, 1, 2, 'writer', 'departed', 6);`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := Open(ctx, root); err == nil {
		got.Close()
		t.Fatal("migration accepted ambiguous historical bindings")
	} else if !strings.Contains(err.Error(), "principal 1 with bindings 1, 4") {
		t.Fatalf("migration did not identify the conflicting bindings: %v", err)
	}
	if failed, err := openWithMigrations(ctx, root, migrations[:22]); err == nil {
		failed.Close()
		t.Fatal("the schema-22 dispatcher opened the partially upgraded database")
	} else if !strings.Contains(err.Error(), "unsupported schema version 26") {
		t.Fatalf("last good migration: %v", err)
	}
	backups, err := filepath.Glob(filepath.Join(root, "store-v22-*.sqlite3"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("migration backup: %v, %v", backups, err)
	}
	backup, err := os.ReadFile(backups[0])
	if err != nil {
		t.Fatal(err)
	}
	restoreRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(restoreRoot, "store.sqlite3"), backup, 0600); err != nil {
		t.Fatal(err)
	}
	restored, err := openWithMigrations(ctx, restoreRoot, migrations[:22])
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	var bindings int
	if err := restored.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM binding`).Scan(&bindings); err != nil || bindings != 4 {
		t.Fatalf("restored %d bindings, want 4: %v", bindings, err)
	}
}

func TestSessionMoveSchemaFencesLiveSelection(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedAdapterLedger(t, db)
	if _, err := db.db.ExecContext(ctx, `INSERT INTO channel (id, name, created_at_ms) VALUES (2, 'side', 1)`); err != nil {
		t.Fatal(err)
	}
	const b = `INSERT INTO binding (id, principal_id, channel_id, alias, joined_at_ms) VALUES (4, 1, 2, 'writer', 2)`
	if _, err := db.db.ExecContext(ctx, b); err == nil {
		t.Fatal("created two live bindings for one principal")
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE member_selection SET binding_id = 2 WHERE principal_id = 1`); err == nil {
		t.Fatal("selected another principal's binding")
	}
	if _, err := db.db.ExecContext(ctx, `UPDATE binding SET state = 'departed' WHERE id = 1;`+b+`;
		UPDATE binding SET state = 'departed' WHERE id = 4; UPDATE binding SET state = 'live', joined_at_ms = 3 WHERE id = 1;
		UPDATE binding SET state = 'departed' WHERE id = 1; UPDATE binding SET state = 'live', joined_at_ms = 4 WHERE id = 1;`); err != nil {
		t.Fatal(err)
	}
	var binding, generation, selectedAt int64
	if err := db.db.QueryRowContext(ctx, `SELECT binding_id, generation, selected_at_ms FROM member_selection WHERE principal_id = 1`).Scan(&binding, &generation, &selectedAt); err != nil || binding != 1 || generation != 3 || selectedAt != 3 {
		t.Fatalf("after A→B→A and a same-channel rejoin: binding %d, generation %d, selected at %d: %v", binding, generation, selectedAt, err)
	}
}
