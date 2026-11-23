package store

import (
	"testing"
)

func TestPublishSwitchMigrationKeepsSuspensionsAndOpenLanes(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	prior, err := openWithMigrations(ctx, root, migrations[:9])
	if err != nil {
		t.Fatal(err)
	}
	seedAdapterLedger(t, prior)
	before := adapterLedgerSnapshot(t, prior.db, 9)
	// One launch with an absorbed, an unknown and an absorbed notice, and a
	// submitted notice whose receipt has not arrived: its lane is held.
	if _, err := prior.db.ExecContext(ctx, `
		INSERT INTO publish_attempt (id, binding_id, principal_id, binding_epoch, generation, adapter, session_ref, launch_ref,
			policy_epoch, process_pid, process_start_ms, state, reserved_at_ms, submitting_at_ms, resolved_at_ms, observed_at_ms, observation)
		VALUES (1, 2, 2, 1, 1, 'claude', 's', 'launch', 1, 7, 1, 'submitted', 10, 10, 10, 100, 'absorbed'),
		       (2, 2, 2, 1, 1, 'claude', 's', 'launch', 1, 7, 1, 'uncertain', 20, 20, 20, 200, 'unknown'),
		       (3, 2, 2, 1, 1, 'claude', 's', 'launch', 1, 7, 1, 'submitted', 30, 30, 30, 300, 'absorbed'),
		       (4, 2, 2, 1, 1, 'claude', 's', 'launch', 1, 7, 1, 'submitted', 40, 40, 40, NULL, NULL),
		       (5, 3, 3, 1, 1, 'claude', 's', 'other', 1, 8, 1, 'submitted', 50, 50, 50, 500, 'own_turn')`); err != nil {
		t.Fatal(err)
	}
	if err := prior.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := openWithMigrations(ctx, root, migrations[:10])
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	if after := adapterLedgerSnapshot(t, upgraded.db, 10); after != before {
		t.Fatalf("ledger changed from %s to %s", before, after)
	}
	rows, err := upgraded.db.QueryContext(ctx, `SELECT binding_id, launch_ref, reason, notice, at_ms, COALESCE(until_ms, -1)
		FROM publish_suspension ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type row struct {
		binding        int64
		launch, reason string
		notice, at, to int64
	}
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.binding, &r.launch, &r.reason, &r.notice, &r.at, &r.to); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	want := []row{
		{2, "launch", "absorbed", 1, 100, 1800100},
		{2, "launch", "unknown", 2, 200, 1800200},
		{2, "launch", "absorbed", 3, 300, -1},
	}
	if len(got) != len(want) {
		t.Fatalf("suspensions %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("suspension %d: %+v, want %+v", i, got[i], want[i])
		}
	}
	var open int
	if err := upgraded.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM publish_attempt
		WHERE id = 4 AND observed_at_ms IS NULL AND retired_at_ms IS NULL AND state = 'submitted'`).Scan(&open); err != nil || open != 1 {
		t.Fatalf("the notice awaiting its receipt lost its lane: %d (%v)", open, err)
	}
}

func TestPublishRecoveryHintMigrationKeepsLegacyLossAndMakesHintPending(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	prior, err := openWithMigrations(ctx, root, migrations[:22])
	if err != nil {
		t.Fatal(err)
	}
	seedAdapterLedger(t, prior)
	if _, err := prior.db.ExecContext(ctx, `UPDATE binding SET reach_told = 0, reach_seen = 0 WHERE id = 2;
		INSERT INTO publish_suspension (id, binding_id, launch_ref, reason, notice, at_ms) VALUES (5, 2, 'launch', 'absorbed', 8, 100);`); err != nil {
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
	var told, seen, hint int
	var lasting bool
	if err := upgraded.db.QueryRowContext(ctx, `SELECT b.reach_told, b.reach_seen, s.recovery_hint, s.until_ms IS NULL
		FROM binding b JOIN publish_suspension s ON s.binding_id = b.id WHERE s.id = 5`).Scan(&told, &seen, &hint, &lasting); err != nil {
		t.Fatal(err)
	}
	if told != 0 || seen != 0 || hint != RecoveryHintPending || !lasting {
		t.Fatalf("after upgrade: reach told=%d, seen=%d, hint=%d, lasting=%t", told, seen, hint, lasting)
	}
}
