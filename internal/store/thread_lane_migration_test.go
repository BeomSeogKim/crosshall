package store

import "testing"

// An open notice recorded before lanes keeps its process's lane, so the
// one-notice-per-queue rule holds across the upgrade.
func TestThreadLaneMigrationKeepsEachOpenNoticeOnItsProcess(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	prior, err := openWithMigrations(ctx, root, migrations[:len(migrations)-1])
	if err != nil {
		t.Fatal(err)
	}
	seedAdapterLedger(t, prior)
	if _, err := prior.db.ExecContext(ctx, `INSERT INTO publish_attempt (binding_id, principal_id, binding_epoch, generation,
		adapter, session_ref, launch_ref, policy_epoch, process_pid, process_start_ms, state, reserved_at_ms)
		VALUES (2, 2, 1, 1, 'claude', 'reader', 'pid-22-start', 1, 22, 500, 'submitted', 10)`); err != nil {
		t.Fatal(err)
	}
	if err := prior.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	var lane string
	if err := upgraded.db.QueryRowContext(ctx, `SELECT lane FROM publish_attempt`).Scan(&lane); err != nil || lane != "process:22:500" {
		t.Fatalf("lane %q (%v)", lane, err)
	}
	insert := `INSERT INTO publish_attempt (binding_id, principal_id, binding_epoch, generation, adapter, session_ref, launch_ref,
		policy_epoch, process_pid, process_start_ms, state, reserved_at_ms, lane)
		VALUES (3, 3, 1, 1, 'codex', 'thread', 'session', 1, 22, 500, 'reserved', 20, ?)`
	if _, err := upgraded.db.ExecContext(ctx, insert, lane); err == nil {
		t.Fatal("a second open notice took the same lane")
	}
	if _, err := upgraded.db.ExecContext(ctx, insert, "thread:codex:thread"); err != nil {
		t.Fatalf("a thread on the same process has its own lane: %v", err)
	}
}
