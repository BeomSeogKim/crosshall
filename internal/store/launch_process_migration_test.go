package store

import "testing"

func TestLaunchProcessMigrationBackfillsOnlyMatchingProof(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	v14, err := openWithMigrations(ctx, root, migrations[:14])
	if err != nil {
		t.Fatal(err)
	}
	seedAdapterLedger(t, v14)
	if _, err := v14.db.ExecContext(ctx, `INSERT INTO adapter_launch
		(principal_id, launch_ref, profile, verified, started_hook_at_ms) VALUES
		(2, 'pid-22-current', '{"adapter":"claude"}', 1, 10),
		(2, 'pid-22-overlap', '{"adapter":"claude"}', 1, 9),
		(3, 'pid-33-newer', '{"adapter":"claude"}', 1, 10);
		INSERT INTO publish_route
		(binding_id, principal_id, generation, adapter, session_ref, launch_ref, launch_verified,
		policy_epoch, mode, watermark_message_id, state, registered_at_ms, updated_at_ms) VALUES
		(2, 2, 5, 'claude', 'reader', 'pid-22-current', 1, 1, 'default', 0, 'active', 10, 10),
		(3, 3, 5, 'claude', 'reviewer', 'pid-33-newer', 1, 1, 'default', 0, 'active', 10, 10);
		INSERT INTO publish_route_proof (binding_id, generation, process_pid, process_start_ms) VALUES
		(2, 5, 22, 500), (3, 4, 33, 600);`); err != nil {
		t.Fatal(err)
	}
	if err := v14.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	var matched, untouched int
	if err := upgraded.db.QueryRowContext(ctx, `SELECT
		COUNT(*) FILTER (WHERE launch_ref = 'pid-22-current' AND process_pid = 22 AND process_start_identity = 500 AND ended_by_process = 0 AND pid_namespace = ''),
		COUNT(*) FILTER (WHERE launch_ref IN ('pid-22-overlap', 'pid-33-newer') AND process_pid IS NULL AND process_start_identity IS NULL AND ended_by_process = 0 AND pid_namespace = '')
		FROM adapter_launch`).Scan(&matched, &untouched); err != nil || matched != 1 || untouched != 2 {
		t.Fatalf("backfill matched %d launch and left %d unrelated launches untouched (%v)", matched, untouched, err)
	}
}
