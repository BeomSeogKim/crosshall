package store

import "testing"

// An unknown receipt observed before causes were recorded keeps none; after
// the upgrade a cause is accepted only on an unknown receipt, and only one
// of the four.
func TestUnknownCauseMigrationLeavesEarlierReceiptsWithoutACause(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	prior, err := openWithMigrations(ctx, root, migrations[:25])
	if err != nil {
		t.Fatal(err)
	}
	seedAdapterLedger(t, prior)
	if _, err := prior.db.ExecContext(ctx, `INSERT INTO publish_attempt (binding_id, principal_id, binding_epoch, generation,
		adapter, session_ref, launch_ref, policy_epoch, process_pid, process_start_ms, state, reserved_at_ms,
		observed_at_ms, observation)
		VALUES (2, 2, 1, 1, 'claude', 'reader', 'pid-22-start', 1, 22, 500, 'submitted', 10, 20, 'unknown')`); err != nil {
		t.Fatal(err)
	}
	if err := prior.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := openWithMigrations(ctx, root, migrations[:26])
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	var n int
	if err := upgraded.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM publish_attempt
		WHERE observation = 'unknown' AND unknown_cause IS NULL`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("earlier unknown receipts without a cause: %d (%v)", n, err)
	}
	set := `UPDATE publish_attempt SET observation = ?, unknown_cause = ?`
	for _, refused := range [][2]string{{"own_turn", "open_placeholder"}, {"unknown", "lost_callback"}} {
		if _, err := upgraded.db.ExecContext(ctx, set, refused[0], refused[1]); err == nil {
			t.Errorf("stored cause %q on a %q receipt", refused[1], refused[0])
		}
	}
	for _, cause := range []string{"open_placeholder", "unfinished_turn", "activity_since_stop", "no_turn_but_facts"} {
		if _, err := upgraded.db.ExecContext(ctx, set, "unknown", cause); err != nil {
			t.Errorf("cause %q refused: %v", cause, err)
		}
	}
}
