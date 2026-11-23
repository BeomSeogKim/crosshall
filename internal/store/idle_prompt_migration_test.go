package store

import (
	"slices"
	"testing"
)

// Migration 027 rebuilds observation to admit the idle_prompt kind: every
// row keeps its sequence and fields, the indexes come back under their
// names, and any other new kind is still refused.
func TestIdlePromptMigrationKeepsObservationsAndAdmitsTheKind(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	prior, err := openWithMigrations(ctx, root, migrations[:26])
	if err != nil {
		t.Fatal(err)
	}
	seedAdapterLedger(t, prior)
	if _, err := prior.db.ExecContext(ctx, `INSERT INTO observation (principal_id, kind, origin, source, observed_at_ms,
		fresh_until_ms, launch_ref, event_ref, turn_ref, hook_at_ms, profile)
		VALUES (2, 'turn_finishing', 'unknown', 'claude', 30, 40, 'pid-22-start', 'stop:1', 'turn-1', 29, '{"adapter":"claude"}')`); err != nil {
		t.Fatal(err)
	}
	const rows = `SELECT json_group_array(json_array(seq, principal_id, kind, origin, source, observed_at_ms,
		fresh_until_ms, launch_ref, event_ref, turn_ref, hook_at_ms, profile) ORDER BY seq) FROM observation`
	var before string
	if err := prior.db.QueryRowContext(ctx, rows).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if err := prior.Close(); err != nil {
		t.Fatal(err)
	}
	upgraded, err := openWithMigrations(ctx, root, migrations[:27])
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	var after string
	if err := upgraded.db.QueryRowContext(ctx, rows).Scan(&after); err != nil || after != before {
		t.Fatalf("observations after the upgrade:\n%s\nwant\n%s (%v)", after, before, err)
	}
	index, err := upgraded.db.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = 'observation'
		AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for index.Next() {
		var name string
		if err := index.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	index.Close()
	if want := []string{"observation_event", "observation_last_command", "observation_principal_sequence"}; !slices.Equal(names, want) {
		t.Fatalf("observation indexes %v, want %v", names, want)
	}
	insert := `INSERT INTO observation (principal_id, kind, origin, source, observed_at_ms, launch_ref, event_ref, hook_at_ms)
		VALUES (2, ?, 'unknown', 'claude', 50, 'pid-22-start', ?, 49)`
	if _, err := upgraded.db.ExecContext(ctx, insert, "idle_prompt", "idle:49"); err != nil {
		t.Fatalf("idle_prompt refused: %v", err)
	}
	if _, err := upgraded.db.ExecContext(ctx, insert, "idle_prompt", "idle:49"); err == nil {
		t.Error("a second copy of one event was stored")
	}
	if _, err := upgraded.db.ExecContext(ctx, insert, "auth_success", "auth:49"); err == nil {
		t.Error("an unknown kind was stored")
	}
}
