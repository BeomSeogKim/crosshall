package store

import (
	"errors"
	"testing"
)

func TestAdapterDirtyTracksEffectiveChanges(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedAdapterLedger(t, db)
	updates := []struct {
		name, sql, want string
	}{
		{"shell clients", `INSERT INTO recipient(message_id, principal_id, alias_at_commit, owes)
		    VALUES (2, 2, 'reader', 0)`, `[]`},
		{"start", `INSERT INTO adapter_launch(principal_id, launch_ref, profile, verified, started_hook_at_ms, ended_hook_at_ms)
		    VALUES (1, 'a', '{"adapter":"claude"}', 1, 100, NULL), (2, 'b', '{"adapter":"codex"}', 0, 100, NULL),
		           (3, 'c', '{"adapter":"claude"}', 1, 100, 200)`, `[[1,1,0],[2,1,0]]`},
		{"end", `UPDATE adapter_launch SET ended_hook_at_ms = 300 WHERE principal_id = 2`, `[]`},
		{"restore launch", `UPDATE adapter_launch SET ended_hook_at_ms = NULL WHERE principal_id = 2`, `[[2,1,0]]`},
		{"no-op launch update", `UPDATE adapter_launch SET started_hook_at_ms = 100 WHERE principal_id = 2`, `[]`},
		{"policy and new mail", `INSERT INTO adapter_policy(adapter, enabled, reminder, epoch, changed_at_ms)
		    VALUES ('claude', 1, 0, 1, 500);
		    INSERT INTO message(id, channel_id, sender_id, kind, audience_mode, body, created_at_ms)
		        VALUES (3, 1, 2, 'note', 'explicit', 'Updated totals', 500);
		    INSERT INTO recipient(message_id, principal_id, alias_at_commit, owes, picked_up_at_ms)
		        VALUES (3, 1, 'author', 0, NULL), (3, 2, 'reader', 0, 500), (3, 3, 'reviewer', 0, NULL)`, `[[1,1,1]]`},
		{"policy change", `UPDATE adapter_policy SET enabled = 0, epoch = 2 WHERE adapter = 'claude'`, `[[1,1,1]]`},
		{"timestamp-only policy update", `UPDATE adapter_policy SET changed_at_ms = 800 WHERE adapter = 'claude'`, `[]`},
		{"hold", `UPDATE binding SET dnd = 1 WHERE principal_id = 2`, `[]`},
		{"clear hold", `UPDATE binding SET dnd = 0 WHERE principal_id = 2`, `[[2,1,0]]`},
		{"depart", `UPDATE binding SET state = 'departed' WHERE principal_id = 2`, `[]`},
		{"rejoin", `UPDATE binding SET state = 'live' WHERE principal_id = 2`, `[[2,1,0]]`},
	}
	for _, update := range updates {
		t.Run(update.name, func(t *testing.T) {
			if err := db.Update(ctx, func(tx Tx) error {
				if _, err := tx.ExecContext(ctx, update.sql); err != nil {
					return err
				}
				checkAdapterDirty(t, tx, update.want)
				_, err := tx.ExecContext(ctx, "DELETE FROM adapter_dirty")
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}

	rollback := errors.New("discard launch change")
	if err := db.Update(ctx, func(tx Tx) error {
		if _, err := tx.ExecContext(ctx, "UPDATE adapter_launch SET started_hook_at_ms = 900 WHERE principal_id = 2"); err != nil {
			return err
		}
		checkAdapterDirty(t, tx, `[[2,1,0]]`)
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatalf("expected rollback, got %v", err)
	}
	checkAdapterDirty(t, db.db, `[]`)
}

func TestAdapterDirtyTracksLiveSessionAcrossStaleEnd(t *testing.T) {
	ctx := t.Context()
	db, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedAdapterLedger(t, db)
	if err := db.Update(ctx, func(tx Tx) error {
		_, err := tx.ExecContext(ctx, `
			INSERT INTO adapter_launch(principal_id, launch_ref, profile, verified, started_hook_at_ms, ended_hook_at_ms)
			VALUES
				(1, 'session', '{"adapter":"codex"}', 0, 100, 200),
				(2, 'session', '{"adapter":"codex"}', 0, 100, 200),
				(3, 'session', '{"adapter":"codex"}', 0, 100, 200),
				(3, 'verified', '{"adapter":"codex"}', 1, 300, 400);
			UPDATE binding SET state = 'departed' WHERE principal_id = 2;
			INSERT INTO message(id, channel_id, sender_id, kind, audience_mode, body, created_at_ms)
			VALUES (3, 1, 3, 'note', 'explicit', 'Mail after resumed session', 500);
			INSERT INTO recipient(message_id, principal_id, alias_at_commit, owes)
			VALUES (3, 1, 'author', 0), (3, 2, 'reader', 0), (3, 3, 'reviewer', 0);`)
		if err != nil {
			return err
		}
		checkAdapterDirty(t, tx, `[[1,1,0]]`)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func checkAdapterDirty(t *testing.T, tx Tx, want string) {
	t.Helper()
	var got string
	err := tx.QueryRowContext(t.Context(), `SELECT json_group_array(json_array(principal_id, notice, reminder)
	    ORDER BY principal_id) FROM adapter_dirty`).Scan(&got)
	if err != nil || got != want {
		t.Fatalf("dirty flags: got %s, want %s (%v)", got, want, err)
	}
}
