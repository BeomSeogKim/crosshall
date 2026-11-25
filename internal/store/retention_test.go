package store

import (
	"testing"
	"time"
)

func retentionStore(t *testing.T) *Store {
	t.Helper()
	db, err := Open(t.Context(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	seedAdapterLedger(t, db)
	return db
}

func (s *Store) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.View(t.Context(), func(tx Tx) error { return tx.QueryRowContext(t.Context(), query, args...).Scan(&n) }); err != nil {
		t.Fatal(err)
	}
	return n
}

// A pass deletes in write transactions of at most its chunk size, one per
// call, so a hook waits for one chunk, not for the whole pass (#256).
func TestRetentionDeletesOneBoundedChunkPerCall(t *testing.T) {
	ctx := t.Context()
	db := retentionStore(t)
	now := time.UnixMilli(1_900_000_000_000)
	old := now.Add(-RetentionWindow - 25*time.Hour).UnixMilli()
	if err := db.Update(ctx, func(tx Tx) error {
		_, err := tx.ExecContext(ctx, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < 7)
			INSERT INTO op (principal_id, op_key, scope, verb, payload_hash, result, at_ms)
			SELECT 2, 'old-' || i, 'channel:dev', 'note', '', '{}', ?1 FROM n
			UNION ALL SELECT 2, 'fresh', 'channel:dev', 'note', '', '{}', ?2`, old, now.UnixMilli())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before := db.count(t, `SELECT COUNT(*) FROM op`)
	pass, err := db.StartRetention(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	pass.limit = 3
	var deleted []int
	for done := false; !done; {
		if done, err = db.RetentionChunk(ctx, pass); err != nil {
			t.Fatal(err)
		}
		left := db.count(t, `SELECT COUNT(*) FROM op`)
		deleted = append(deleted, before-left)
		before = left
	}
	if got := pass.Result.Ops; got < 7 {
		t.Fatalf("deleted %d ops, want the 7 old ones", got)
	}
	if deleted[0] != 3 || deleted[1] != 3 {
		t.Fatalf("ops deleted per call %v, want 3 and 3 first", deleted)
	}
	for _, n := range deleted {
		if n > 3 {
			t.Fatalf("one call deleted %d ops, over the chunk of 3: %v", n, deleted)
		}
	}
	if db.count(t, `SELECT COUNT(*) FROM op WHERE op_key = 'fresh'`) != 1 {
		t.Fatal("the fresh op is gone")
	}
}

// Old facts of running launches are never deleted, so every pass meets
// them again; a chunk must not scan a principal's history for each one.
// Before the running set was read once per pass, 31 members with 1,000
// such facts each took 13 seconds for one chunk; three with 3,000 here took
// several.
func TestRetentionChunkStaysFastWithOldFactsOfRunningLaunches(t *testing.T) {
	ctx := t.Context()
	db := retentionStore(t)
	now := time.UnixMilli(1_900_000_000_000)
	old := now.Add(-RetentionWindow - 25*time.Hour).UnixMilli()
	const perMember = 3_000
	if err := db.Update(ctx, func(tx Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO adapter_launch (principal_id, launch_ref, profile, verified, started_hook_at_ms)
			SELECT id, 'pid-' || id, '{"adapter":"claude"}', 1, ?1 - 1 FROM principal`, old); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?2)
			INSERT INTO observation (principal_id, kind, origin, source, observed_at_ms, launch_ref, event_ref, turn_ref, hook_at_ms)
			SELECT p.id, CASE i % 2 WHEN 0 THEN 'prompt_submitted' ELSE 'turn_finishing' END, 'unknown', 'claude', ?1,
				'pid-' || p.id, 'event-' || i, 'turn-' || (i / 2), ?1 FROM principal p, n`, old, perMember)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	facts := db.count(t, `SELECT COUNT(*) FROM observation WHERE launch_ref IS NOT NULL`)
	pass, err := db.StartRetention(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	var slowest time.Duration
	for done := false; !done; {
		start := time.Now()
		if done, err = db.RetentionChunk(ctx, pass); err != nil {
			t.Fatal(err)
		}
		slowest = max(slowest, time.Since(start))
	}
	if n := db.count(t, `SELECT COUNT(*) FROM observation WHERE launch_ref IS NOT NULL`); n != facts {
		t.Fatalf("%d of %d facts of running launches left", n, facts)
	}
	if slowest > time.Second {
		t.Fatalf("the slowest chunk took %s over %d facts of running launches", slowest, facts)
	}
}

// A pass that deletes facts or turns clears the reassignment record of every
// launch it may delete from, so the next reassignment reads all that is left;
// a running launch keeps its facts and its record (#366).
func TestRetentionClearsTheReassignmentRecordOfLaunchesItDeletesFrom(t *testing.T) {
	ctx := t.Context()
	db := retentionStore(t)
	now := time.UnixMilli(1_900_000_000_000)
	old := now.Add(-RetentionWindow - 25*time.Hour).UnixMilli()
	if err := db.Update(ctx, func(tx Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO adapter_launch (principal_id, launch_ref, profile, verified, started_hook_at_ms,
				ended_hook_at_ms, reconciled_seq, reconciled_at_ms)
			SELECT id, 'running-' || id, '{"adapter":"claude"}', 1, ?1 - 1, NULL, 1, ?1 FROM principal
			UNION ALL SELECT id, 'ended-' || id, '{"adapter":"claude"}', 1, ?1 - 1, ?1, 1, ?1 FROM principal`, old); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO observation (principal_id, kind, origin, source, observed_at_ms, launch_ref, event_ref, turn_ref, hook_at_ms)
			SELECT p.id, 'prompt_submitted', 'unknown', 'claude', ?1, l.launch_ref, 'event', 'turn', ?1
			FROM principal p JOIN adapter_launch l ON l.principal_id = p.id`, old)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	recorded := `SELECT COUNT(*) FROM adapter_launch WHERE launch_ref LIKE ? AND reconciled_seq IS NOT NULL`
	launches := db.count(t, recorded, "running-%")
	if launches == 0 || db.count(t, recorded, "ended-%") != launches {
		t.Fatalf("seeded %d running launches, %d ended", launches, db.count(t, recorded, "ended-%"))
	}
	pass, err := db.StartRetention(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	for done := false; !done; {
		if done, err = db.RetentionChunk(ctx, pass); err != nil {
			t.Fatal(err)
		}
	}
	if pass.Result.Observations == 0 {
		t.Fatal("the pass deleted no facts")
	}
	if n := db.count(t, recorded, "ended-%"); n != 0 {
		t.Fatalf("%d ended launches lost facts and kept their record", n)
	}
	if n := db.count(t, recorded, "running-%"); n != launches {
		t.Fatalf("%d of %d running launches kept their record", n, launches)
	}
}
