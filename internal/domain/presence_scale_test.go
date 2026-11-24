package domain

import (
	"database/sql"
	"fmt"
	"math/rand/v2"
	"strconv"
	"testing"
	"time"
)

// quadraticLatestFact is the effective-fact query before #186, kept as the
// reference the current one must agree with.
const quadraticLatestFact = `
	WITH latest AS (SELECT launch_ref, started_hook_at_ms FROM adapter_launch WHERE principal_id = ?1
	                ORDER BY ended_hook_at_ms IS NOT NULL, started_hook_at_ms DESC, rowid DESC LIMIT 1),
	candidate AS (
		SELECT o.seq, o.kind, o.profile, o.hook_at_ms, o.fresh_until_ms, o.turn_ref, o.launch_ref,
		       CASE o.kind WHEN 'session_ended' THEN 4 WHEN 'turn_finishing' THEN 3
		                   WHEN 'approval_requested' THEN 2 ELSE 1 END AS stage,
		       COALESCE((SELECT t.opened_hook_at_ms FROM turn t WHERE t.principal_id = o.principal_id
		                 AND t.launch_ref = o.launch_ref AND t.turn_ref = o.turn_ref), o.hook_at_ms) AS position
		FROM observation o JOIN latest ON o.launch_ref = latest.launch_ref
		WHERE o.principal_id = ?1 AND o.kind <> 'command'
		  AND o.hook_at_ms <= o.observed_at_ms + ?2)
	SELECT c.kind, c.profile, c.hook_at_ms, c.fresh_until_ms FROM candidate c
	WHERE c.position >= (SELECT started_hook_at_ms FROM latest) AND NOT EXISTS (SELECT 1 FROM candidate s
	                  WHERE c.turn_ref IS NOT NULL AND s.turn_ref = c.turn_ref AND s.stage > c.stage)
	ORDER BY c.position DESC, c.stage DESC, c.hook_at_ms DESC, c.seq DESC
	LIMIT 1`

// #186: the effective fact is the same as the reference query's over
// random launches, starts, turns and facts, including skewed hook clocks,
// facts without a turn and turns shared by several facts.
func TestLatestFactAgreesWithTheReferenceQuery(t *testing.T) {
	f := newFixture(t)
	r := rand.New(rand.NewPCG(186, 186))
	kinds := []string{"command", "prompt_submitted", "turn_finishing", "approval_requested", "session_ended"}
	for run := range 300 {
		principal := int64(run + 1)
		exec := func(query string, args ...any) {
			t.Helper()
			if _, err := f.db.Exec(query, args...); err != nil {
				t.Fatalf("run %d: %v", run, err)
			}
		}
		exec(`INSERT INTO principal (id, kind, vendor, created_at_ms) VALUES (?, 'session', 'claude', 1)`, principal)
		launches := 1 + r.IntN(3)
		for l := range launches {
			var ended any
			if r.IntN(3) == 0 {
				ended = 1000 + r.Int64N(1000)
			}
			exec(`INSERT INTO adapter_launch (principal_id, launch_ref, profile, verified, started_hook_at_ms, ended_hook_at_ms)
				VALUES (?, ?, '{"adapter":"claude"}', 1, ?, ?)`, principal, "l"+strconv.Itoa(l), r.Int64N(500), ended)
		}
		for turn := range 1 + r.IntN(6) {
			if r.IntN(4) > 0 {
				exec(`INSERT INTO turn (principal_id, launch_ref, turn_ref, opened_hook_at_ms) VALUES (?, ?, ?, ?)`,
					principal, "l"+strconv.Itoa(r.IntN(launches)), "t"+strconv.Itoa(turn), r.Int64N(1500))
			}
		}
		for e := range 1 + r.IntN(25) {
			var turn any
			if r.IntN(4) > 0 {
				turn = "t" + strconv.Itoa(r.IntN(6))
			}
			hook := r.Int64N(1500)
			observed := hook + r.Int64N(2*futureSlackMS) - futureSlackMS
			exec(`INSERT INTO observation (principal_id, kind, origin, source, observed_at_ms, fresh_until_ms, launch_ref, event_ref, turn_ref, hook_at_ms, profile)
				VALUES (?, ?, 'unknown', 'hook', ?, ?, ?, ?, ?, ?, ?)`, principal, kinds[r.IntN(len(kinds))], observed, hook+r.Int64N(100),
				"l"+strconv.Itoa(r.IntN(launches)), "e"+strconv.Itoa(e), turn, hook, fmt.Sprintf(`{"adapter":"a%d"}`, e))
		}
		var want ObservedFact
		var wantFresh sql.NullInt64
		var wantProfile string
		err := f.db.QueryRow(quadraticLatestFact, principal, futureSlackMS).Scan(&want.Fact, &wantProfile, &want.AtMS, &wantFresh)
		if err != nil && err != sql.ErrNoRows {
			t.Fatal(err)
		}
		got, gotErr := latestFact(t.Context(), f.db, principal)
		if gotErr != nil {
			t.Fatal(gotErr)
		}
		switch {
		case err == sql.ErrNoRows && got != nil:
			t.Fatalf("run %d: got %+v, reference has no fact", run, *got)
		case err == nil && (got == nil || got.Fact != want.Fact || got.AtMS != want.AtMS || got.Source != adapterOf(wantProfile) ||
			(got.FreshUntilMS == nil) != !wantFresh.Valid || (got.FreshUntilMS != nil && *got.FreshUntilMS != wantFresh.Int64)):
			t.Fatalf("run %d: got %+v, reference %+v from %s", run, got, want, wantProfile)
		}
	}
}

// #186: hooks have a one-second timeout, so a hook's work must not grow
// with the square of a long session's history.
func TestHookCostStaysBoundedInALongSession(t *testing.T) {
	if testing.Short() {
		t.Skip("timing")
	}
	var at250, at500 time.Duration
	for _, turns := range []int{250, 400, 500} {
		f, reader, _ := turnEndFixture(t, "launch")
		at := f.now.UnixMilli() + 10
		for i := range turns {
			at++
			f.hook(reader, typedPrompt("t"+strconv.Itoa(i), at))
			at++
			f.hook(reader, stop("launch", "t"+strconv.Itoa(i), at))
		}
		start := time.Now()
		for i := range 20 {
			at++
			f.hook(reader, typedPrompt("u"+strconv.Itoa(i), at))
		}
		per := time.Since(start) / 20
		t.Logf("%d turns: %v per hook event", turns, per)
		if turns == 250 {
			at250 = per
		}
		at500 = per
	}
	if at500 > 100*time.Millisecond {
		t.Fatalf("a hook event after 500 turns took %v, want under 100ms", at500)
	}
	// The bound alone let a cost six times higher pass on a fast machine.
	// A hook event reads the session's facts, so its cost may grow with the
	// turns (about 1.9 times from 250 to 500 turns), but not faster: work
	// done per earlier fact made it 4.6 times.
	if ratio := float64(at500) / float64(at250); ratio > 2.5 {
		t.Fatalf("a hook event after 500 turns took %v, %.1f times the %v after 250: the cost grows faster than the turns", at500, ratio, at250)
	}
}
