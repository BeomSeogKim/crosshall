package store

import (
	"fmt"
	"testing"
)

func TestLoopGuardMigrationPreservesLedgerAndInitializesNoPositions(t *testing.T) {
	ctx, root := t.Context(), t.TempDir()
	old, err := openWithMigrations(ctx, root, migrations[:20])
	if err != nil {
		t.Fatal(err)
	}
	seedAdapterLedger(t, old)
	before := adapterLedgerSnapshot(t, old.db, 20)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	upgraded, err := openWithMigrations(ctx, root, migrations[:21])
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	if after := adapterLedgerSnapshot(t, upgraded.db, 21); after != before {
		t.Fatalf("migration changed the ledger\nbefore: %s\nafter: %s", before, after)
	}
	var n int
	if err := upgraded.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM message WHERE level_ms <> 0 OR thread_root_id IS NOT NULL
			OR thread_reset_after_message_id <> 0 OR thread_round_limit <> 8) +
		(SELECT COUNT(*) FROM principal WHERE any_level_ms <> 0 OR reset_after_message_id <> 0 OR round_limit <> 8 OR guard_told <> 0) +
		(SELECT COUNT(*) FROM thread_position)`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("migration invented guard positions: rows=%d err=%v", n, err)
	}
}

func TestLoopGuardCutoverFencesBothUpgradePaths(t *testing.T) {
	for _, version := range []int{20, 21} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			ctx, root := t.Context(), t.TempDir()
			old, err := openWithMigrations(ctx, root, migrations[:version])
			if err != nil {
				t.Fatal(err)
			}
			seedAdapterLedger(t, old)
			if _, err := old.db.ExecContext(ctx, `UPDATE exchange SET episode_open = 1, episode_count = 3`); err != nil {
				t.Fatal(err)
			}
			if version == 21 {
				if _, err := old.db.ExecContext(ctx, `UPDATE message SET level_ms = 4800001;
					UPDATE principal SET any_level_ms = 4800001, guard_told = 1;
					INSERT INTO thread_position VALUES (1, 1, 4800001)`); err != nil {
					t.Fatal(err)
				}
			}
			before := adapterLedgerSnapshot(t, old.db, version)
			if err := old.Close(); err != nil {
				t.Fatal(err)
			}
			upgraded, err := openWithMigrations(ctx, root, migrations[:22])
			if err != nil {
				t.Fatal(err)
			}
			defer upgraded.Close()
			if after := adapterLedgerSnapshot(t, upgraded.db, 22); after != before {
				t.Fatal("cutover changed prior ledger rows")
			}
			var wrong int
			err = upgraded.db.QueryRowContext(ctx, `SELECT
				(SELECT COUNT(*) FROM message WHERE kind = 'ask' AND thread_reset_after_message_id <> 2) +
				(SELECT COUNT(*) FROM principal WHERE reset_after_message_id <> 2 OR any_level_ms <> 0 OR guard_told <> 0) +
				(SELECT COUNT(*) FROM exchange WHERE episode_open <> 0 OR episode_count <> 0) +
				(SELECT COUNT(*) FROM thread_position)`).Scan(&wrong)
			if err != nil || wrong != 0 {
				t.Fatalf("cutover retained position or missed a fence: rows=%d err=%v", wrong, err)
			}
			var level int64
			err = upgraded.db.QueryRowContext(ctx, `SELECT level_ms FROM message WHERE id = 1`).Scan(&level)
			if err != nil || version == 20 && level != 0 || version == 21 && level != 4800001 {
				t.Fatalf("cutover overwrote the original message stamp: level=%d err=%v", level, err)
			}
		})
	}
}

func TestOwnerAlertMigrationLeavesEarlierStopsUnannounced(t *testing.T) {
	ctx, root := t.Context(), t.TempDir()
	old, err := openWithMigrations(ctx, root, migrations[:22])
	if err != nil {
		t.Fatal(err)
	}
	seedAdapterLedger(t, old)
	if _, err := old.db.ExecContext(ctx, `UPDATE exchange SET episode_open = 1, episode_count = 1;
		INSERT INTO event (kind, subject, actor_id, detail, at_ms)
		SELECT 'loop_guard_stopped', 'exchange:' || id, NULL, '{"round":9,"round_limit":8}', 1 FROM exchange`); err != nil {
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
	if after := adapterLedgerSnapshot(t, upgraded.db, len(migrations)); after != before {
		t.Fatal("migration 024 changed prior ledger rows")
	}
	var alerts, open int
	if err := upgraded.db.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM owner_alert),
		(SELECT COUNT(*) FROM exchange WHERE episode_open = 1)`).Scan(&alerts, &open); err != nil || alerts != 0 || open == 0 {
		t.Fatalf("earlier stops: %d alerts, %d still open, err %v", alerts, open, err)
	}
}
