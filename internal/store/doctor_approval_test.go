package store

import "testing"

// Doctor names the live members whose turn waits on the owner's
// approval. A command since, a later Stop, an ended launch or a departed
// binding ends it, and a Codex request, which Codex may approve on its own,
// never starts it.
func TestDoctorNamesMembersWaitingOnTheOwnersApproval(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Update(ctx, func(tx Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO channel (id, name, created_at_ms) VALUES (1, 'checkout', 1);
			INSERT INTO principal (id, kind, vendor, created_at_ms) VALUES
				(1, 'session', 'claude', 1), (2, 'session', 'claude', 1), (3, 'session', 'claude', 1),
				(4, 'session', 'claude', 1), (5, 'session', 'claude', 1), (6, 'session', 'codex', 1), (7, 'session', 'claude', 1);
			INSERT INTO binding (id, principal_id, channel_id, alias, state, joined_at_ms) VALUES
				(1, 1, 1, 'blocked', 'live', 1), (2, 2, 1, 'answered', 'live', 1), (3, 3, 1, 'stopped', 'live', 1),
				(4, 4, 1, 'ended', 'live', 1), (5, 5, 1, 'gone', 'departed', 1), (6, 6, 1, 'codex', 'live', 1), (7, 7, 1, 'denied', 'live', 1);
			INSERT INTO adapter_launch (principal_id, launch_ref, profile, verified, started_hook_at_ms, ended_hook_at_ms) VALUES
				(1, 'session', '{"adapter":"claude"}', 0, 10, NULL), (2, 'session', '{"adapter":"claude"}', 0, 10, NULL),
				(3, 'session', '{"adapter":"claude"}', 0, 10, NULL), (4, 'session', '{"adapter":"claude"}', 0, 10, 30),
				(5, 'session', '{"adapter":"claude"}', 0, 10, NULL), (6, 'session', '{"adapter":"codex"}', 0, 10, NULL), (7, 'session', '{"adapter":"claude"}', 0, 10, NULL);
			INSERT INTO observation (principal_id, kind, origin, source, observed_at_ms, launch_ref, turn_ref, hook_at_ms, profile) VALUES
				(1, 'approval_requested', 'unknown', 'hook', 20, 'session', 't1', 20, '{"adapter":"claude"}'),
				(2, 'approval_requested', 'unknown', 'hook', 20, 'session', 't1', 20, '{"adapter":"claude"}'),
				(2, 'command', 'unknown', 'cli', 25, NULL, NULL, NULL, NULL),
				(3, 'approval_requested', 'unknown', 'hook', 20, 'session', 't1', 20, '{"adapter":"claude"}'),
				(3, 'turn_finishing', 'unknown', 'hook', 25, 'session', 't1', 25, '{"adapter":"claude"}'),
				(4, 'approval_requested', 'unknown', 'hook', 20, 'session', 't1', 20, '{"adapter":"claude"}'),
				(5, 'approval_requested', 'unknown', 'hook', 20, 'session', 't1', 20, '{"adapter":"claude"}'),
				(6, 'approval_requested', 'unknown', 'hook', 20, 'session', 't1', 20, '{"adapter":"codex"}');
			INSERT INTO observation (principal_id, kind, origin, source, observed_at_ms, launch_ref, turn_ref, hook_at_ms, profile, fresh_until_ms) VALUES
				(7, 'approval_requested', 'unknown', 'hook', 20, 'session', 't1', 20, '{"adapter":"claude"}', 50);`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.View(ctx, func(tx Tx) error {
		got, err := readDoctorApprovals(ctx, tx, 100)
		if err != nil {
			return err
		}
		// Past its cap a dialog is still listed, as not current: a denied
		// one sends no Stop.
		want := []DoctorApproval{{Alias: "blocked", Channel: "checkout", AtMS: 20, Current: true},
			{Alias: "denied", Channel: "checkout", AtMS: 20}}
		if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("approvals %+v, want %+v", got, want)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
