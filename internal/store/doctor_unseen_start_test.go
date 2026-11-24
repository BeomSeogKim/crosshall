package store

import "testing"

// #252: doctor counts the live members whose current launch has no seen
// start, the ones a prompt joined without a notice route.
func TestDoctorCountsLiveMembersWithoutASeenStart(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Update(ctx, func(tx Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO channel (id, name, created_at_ms) VALUES (1, 'checkout', 1);
			INSERT INTO principal (id, kind, vendor, created_at_ms) VALUES
				(1, 'session', 'claude', 1), (2, 'session', 'claude', 1), (3, 'session', 'claude', 1), (4, 'session', 'claude', 1);
			INSERT INTO binding (principal_id, channel_id, alias, state, joined_at_ms) VALUES
				(1, 1, 'recovered', 'live', 1), (2, 1, 'started', 'live', 1), (3, 1, 'departed', 'departed', 1), (4, 1, 'bound-late', 'live', 1);
			INSERT INTO adapter_launch (principal_id, launch_ref, profile, verified, started_hook_at_ms) VALUES
				(1, 'session', '{"adapter":"claude","vendor_version":"2.1.327","mode":"unverified"}', 0, 0),
				(2, 'pid-2-a', '{"adapter":"claude","vendor_version":"2.1.327","mode":"print"}', 1, 5),
				(3, 'session', '{"adapter":"claude","vendor_version":"2.1.327","mode":"unverified"}', 0, 0),
				(4, 'session', '{"adapter":"claude","vendor_version":"2.1.327","mode":"unverified"}', 0, 0),
				(4, 'pid-4-a', '{"adapter":"claude","vendor_version":"2.1.327","mode":"print"}', 1, 7);`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.View(ctx, func(tx Tx) error {
		var doctor DoctorAdapterState
		if err := readDoctorBuilds(ctx, tx, "claude", &doctor); err != nil {
			return err
		}
		if doctor.UnseenStarts != 1 {
			t.Fatalf("%d live members without a seen start, want 1", doctor.UnseenStarts)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
