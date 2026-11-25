package domain

import (
	"database/sql"
	"flag"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

var reconcileSeed = flag.Uint64("reconcile-seed", 0, "seed for the bounded turn reassignment test (0: from the clock)")

// historyFact is one turn fact in hook order.
type historyFact struct {
	kind, ref, event string
	at               int64
}

// historyMix weighs, per thousand facts, what a history does besides
// prompting new turns and stopping the running one.
type historyMix struct {
	typedAhead, oldPrompt, unknownStop, oldStop, idle int // facts
	tie                                               int // facts in the millisecond of the one before
	late, veryLate, behind, back                      int // deliveries
}

// harshMix stresses every rule of the fold; realMix is closer to what a
// session sends.
var (
	harshMix = historyMix{typedAhead: 150, oldPrompt: 50, unknownStop: 100, oldStop: 50, idle: 100,
		tie: 100, late: 150, veryLate: 50, behind: 50, back: 20}
	realMix = historyMix{typedAhead: 20, oldPrompt: 5, unknownStop: 20, oldStop: 5, idle: 50,
		tie: 20, late: 30, veryLate: 2, behind: 2, back: 2}
)

// turnHistory generates a launch's prompts, Stops and idle prompts in hook
// order: new turns, typed-ahead prompts with the running turn's id, repeats
// of old prompts, Stops of the running turn, of an unknown id and of old
// turns, lost Stops, and ties within a millisecond.
func turnHistory(r *rand.Rand, n int, m historyMix) []historyFact {
	at := int64(1_800_000_000_000)
	var facts []historyFact
	var refs []string
	cur := ""
	newRef := func() string { refs = append(refs, fmt.Sprintf("t%d", len(refs)+1)); return refs[len(refs)-1] }
	old := func() string {
		if len(refs) == 0 {
			return newRef()
		}
		return refs[r.IntN(len(refs))]
	}
	for i := range n {
		switch x := r.IntN(1000); {
		case x < m.tie:
		case x < 2*m.tie:
			at += 1 + int64(r.IntN(3))
		default:
			at += 100 + int64(r.IntN(3000))
		}
		f := historyFact{at: at, event: fmt.Sprintf("e%d", i+1)}
		x, edge := r.IntN(1000), 0
		next := func(w int) bool { edge += w; return x < edge }
		switch {
		case cur == "":
			cur = newRef()
			f.kind, f.ref = FactPromptSubmitted, cur
		case next(m.typedAhead):
			f.kind, f.ref = FactPromptSubmitted, cur
		case next(m.oldPrompt):
			f.kind, f.ref = FactPromptSubmitted, old()
		case next(m.unknownStop):
			f.kind, f.ref = FactTurnFinishing, newRef()
		case next(m.oldStop):
			f.kind, f.ref = FactTurnFinishing, old()
		case next(m.idle):
			f.kind = FactIdlePrompt
		case r.IntN(2) == 0:
			cur = newRef()
			f.kind, f.ref = FactPromptSubmitted, cur
		default:
			f.kind, f.ref = FactTurnFinishing, cur
		}
		facts = append(facts, f)
	}
	return facts
}

type delivered struct {
	historyFact
	now int64 // the dispatcher's clock when it stores the fact
}

// deliverHistory orders a history as the dispatcher stores it: most facts
// at once, some late, and the dispatcher's clock usually just after the
// hook, sometimes more than the slack behind it, sometimes back in time.
func deliverHistory(r *rand.Rand, facts []historyFact, m historyMix) []delivered {
	arrive := make([]int64, len(facts))
	for i, f := range facts {
		switch x := r.IntN(1000); {
		case x < m.late:
			arrive[i] = f.at + int64(r.IntN(3000))
		case x < m.late+m.veryLate:
			arrive[i] = f.at + int64(r.IntN(20000))
		default:
			arrive[i] = f.at
		}
	}
	idx := make([]int, len(facts))
	for i := range idx {
		idx[i] = i
	}
	slices.SortStableFunc(idx, func(a, b int) int { return int(arrive[a] - arrive[b]) })
	out := make([]delivered, len(facts))
	var last int64
	for k, i := range idx {
		d := delivered{historyFact: facts[i]}
		switch x := r.IntN(1000); {
		case x < m.behind:
			d.now = arrive[i] - futureSlackMS - 1 - int64(r.IntN(3000))
		case x < m.behind+m.back:
			d.now = last - int64(r.IntN(2000))
		default:
			d.now = arrive[i] + int64(r.IntN(50))
		}
		last = d.now
		out[k] = d
	}
	return out
}

type reconcileSide struct {
	principal int64
	full      bool // the record is cleared before each reassignment: it folds every fact
}

func (s reconcileSide) rows(t *testing.T, db *sql.DB) []turnState {
	t.Helper()
	rows, err := db.Query(`SELECT turn_ref, opened_hook_at_ms, COALESCE(finishing_hook_at_ms, 0), superseded FROM turn
		WHERE principal_id = ? AND launch_ref = 'L' ORDER BY turn_ref`, s.principal)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []turnState
	for rows.Next() {
		var st turnState
		if err := rows.Scan(&st.name, &st.opened, &st.finishing, &st.superseded); err != nil {
			t.Fatal(err)
		}
		out = append(out, st)
	}
	return out
}

// #366: after every stored fact, reassigning from the recent facts gives
// the turn rows and the result of reassigning from all of them, over
// out-of-order delivery, clocks past the slack or back in time, lost and
// late Stops, typed-ahead chains, idle prompts and claimed turns.
func TestRecentReassignmentEqualsTheFullOne(t *testing.T) {
	seed := *reconcileSeed
	if seed == 0 {
		seed = uint64(time.Now().UnixNano())
	}
	for _, c := range []struct {
		name      string
		mix       historyMix
		histories int
		atLeast   float64 // the share of reassignments that fold only the recent facts
	}{{"harsh", harshMix, 200, 0.25}, {"real", realMix, 40, 0.9}} {
		t.Run(c.name, func(t *testing.T) {
			r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
			calls, bounded := reassignBothWays(t, r, seed, c.mix, c.histories)
			t.Logf("seed %d: %d of %d reassignments folded only the recent facts", seed, bounded, calls)
			if float64(bounded) < c.atLeast*float64(calls) {
				t.Fatalf("seed %d: %d of %d reassignments folded only the recent facts, want at least %.0f%%", seed, bounded, calls, 100*c.atLeast)
			}
		})
	}
}

// reassignBothWays stores histories for two members with the same facts
// and compares their turns after each reassignment.
func reassignBothWays(t *testing.T, r *rand.Rand, seed uint64, mix historyMix, histories int) (calls, bounded int) {
	t.Helper()
	for h := range histories {
		f := newFixture(t)
		sides := []reconcileSide{{principal: 1}, {principal: 2, full: true}}
		for _, s := range sides {
			if _, err := f.db.Exec(`INSERT INTO principal (id, kind, vendor, created_at_ms) VALUES (?, 'session', 'claude', 0)`, s.principal); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.Exec(`INSERT INTO adapter_launch (principal_id, launch_ref, profile, verified, started_hook_at_ms)
				VALUES (?, 'L', '{}', 1, 0)`, s.principal); err != nil {
				t.Fatal(err)
			}
		}
		history := deliverHistory(r, turnHistory(r, 40+r.IntN(160), mix), mix)
		for i, d := range history {
			where := fmt.Sprintf("seed %d history %d fact %d (%s %s at %d, now %d)", seed, h, i, d.kind, d.ref, d.at, d.now)
			var ref any
			if d.ref != "" {
				ref = d.ref
			}
			for _, s := range sides {
				if _, err := f.db.Exec(`INSERT INTO observation (principal_id, kind, origin, source, observed_at_ms, launch_ref, event_ref, turn_ref, hook_at_ms)
					VALUES (?, ?, 'unknown', 'hook', ?, 'L', ?, ?, ?)`, s.principal, d.kind, d.now, d.event, ref, d.at); err != nil {
					t.Fatal(err)
				}
			}
			if r.IntN(10) == 0 {
				continue // stored without a reassignment
			}
			type result struct {
				turn   string
				latest bool
			}
			var got [2]result
			for k, s := range sides {
				tx, err := f.db.Begin()
				if err != nil {
					t.Fatal(err)
				}
				if s.full {
					if _, err := tx.Exec(`UPDATE adapter_launch SET reconciled_seq = NULL, reconciled_at_ms = NULL WHERE principal_id = ?`, s.principal); err != nil {
						t.Fatal(err)
					}
				} else {
					_, _, _, ok, err := recentTurns(t.Context(), tx, s.principal, "L", d.event, d.now)
					if err != nil {
						t.Fatal(err)
					}
					calls++
					if ok {
						bounded++
					}
				}
				got[k].turn, got[k].latest, err = reconcileTurns(t.Context(), tx, s.principal, "L", d.event, d.now)
				if err != nil {
					t.Fatalf("%s: %v", where, err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			}
			if got[0] != got[1] {
				t.Fatalf("%s: recent reassignment returned %+v, the full one %+v", where, got[0], got[1])
			}
			recent, full := sides[0].rows(t, f.db), sides[1].rows(t, f.db)
			if !slices.Equal(recent, full) {
				t.Fatalf("%s:\nrecent %+v\nfull   %+v", where, recent, full)
			}
			// A turn-end notice claims the latest turn now and then: a
			// claimed turn is kept when the assignment drops it.
			if r.IntN(8) == 0 && len(full) > 0 {
				name := full[r.IntN(len(full))].name
				for _, s := range sides {
					if _, err := f.db.Exec(`INSERT OR IGNORE INTO turn_end_notice (principal_id, adapter, launch_ref, turn_ref, at_ms)
						VALUES (?, 'claude', 'L', ?, ?)`, s.principal, name, d.now); err != nil {
						t.Fatal(err)
					}
				}
			}
		}
	}
	return calls, bounded
}
