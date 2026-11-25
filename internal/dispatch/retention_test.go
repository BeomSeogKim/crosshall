package dispatch

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// The retention pass starts once per UTC day, at the first tick of a later
// day, and a clock set back starts no earlier pass (#256).
func TestRetentionRunsOncePerUTCDay(t *testing.T) {
	day := time.Date(2026, 11, 24, 23, 0, 0, 0, time.UTC)
	kst := time.FixedZone("KST", 9*3600)
	for _, c := range []struct {
		name      string
		last, now time.Time
		want      bool
	}{
		{"first pass", time.Time{}, day, true},
		{"same UTC day", day, day.Add(59 * time.Minute), false},
		{"next UTC day", day, day.Add(time.Hour), true},
		{"clock set back", day, day.Add(-48 * time.Hour), false},
		{"local midnight is not UTC", day, day.In(kst).Add(30 * time.Minute), false},
		{"the next UTC day in another zone", day.In(kst), day.Add(90 * time.Minute).In(kst), true},
	} {
		if got := retentionDue(c.last, c.now); got != c.want {
			t.Errorf("%s: due %v, want %v", c.name, got, c.want)
		}
	}
}

// A scan, the startup one included, runs no retention; each loop tick runs
// one chunk, so a pass of more than one chunk spans ticks and a hook waits
// for one chunk at most. A finished pass starts no other that day.
func TestRetentionRunsOneChunkPerTickAndNoneInAScan(t *testing.T) {
	ctx := t.Context()
	root := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	state, err := spool.OpenServerRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	if err := prepareNamedSpool(state, "owner"); err != nil {
		t.Fatal(err)
	}
	s := server{cfg: Config{LaunchConfig: LaunchConfig{StateRoot: root, Rendezvous: filepath.Join(t.TempDir(), "rendezvous")},
		Services: testServices()}, state: state, db: db, ready: readyMarker{Instance: "retention-test"}}
	defer s.withdrawReady()

	// Noon UTC: the tick a minute later is on the same UTC day whenever the
	// test runs.
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	old := now.Add(-store.RetentionWindow - 48*time.Hour).UnixMilli()
	const chunk = 2000
	if err := db.Update(ctx, func(tx store.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO principal (id, kind, vendor, root_tokens, created_at_ms) VALUES (1, 'session', 'route', 0, 1)`); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < ?2)
			INSERT INTO op (principal_id, op_key, scope, verb, payload_hash, result, at_ms)
			SELECT 1, 'old-' || i, 'channel:dev', 'note', '', '{}', ?1 FROM n`, old, chunk+1)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	ops := func() int {
		var n int
		if err := db.View(ctx, func(tx store.Tx) error { return tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM op`).Scan(&n) }); err != nil {
			t.Fatal(err)
		}
		return n
	}

	if err := s.scan(ctx); err != nil {
		t.Fatal(err)
	}
	if n := ops(); n != chunk+1 || s.retention.pass != nil || !s.retention.last.IsZero() {
		t.Fatalf("a scan ran retention: %d ops left, pass %v", n, s.retention.pass)
	}
	s.retentionTick(ctx, now)
	if n := ops(); n != 1 || s.retention.pass == nil {
		t.Fatalf("after one tick %d ops left and pass %v, want one chunk deleted and the pass open", n, s.retention.pass)
	}
	for ticks := 0; s.retention.pass != nil; ticks++ {
		if ticks > len("ops observations turns") {
			t.Fatal("the pass did not finish")
		}
		s.retentionTick(ctx, now)
	}
	if n := ops(); n != 0 {
		t.Fatalf("%d old ops left after the pass", n)
	}
	s.retentionTick(ctx, now.Add(time.Minute))
	if s.retention.pass != nil {
		t.Fatal("a second pass started on the same day")
	}
}
