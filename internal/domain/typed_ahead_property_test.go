package domain

import (
	"flag"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
	"time"
)

var typedAheadSeed = flag.Uint64("typed-ahead-seed", 0, "seed for the typed-ahead delivery property test (0: from the clock)")

// launchShape generates a launch's prompts and Stops in hook order: idle
// turns (also standing for a mail notice's own prompt), single typed-ahead
// turns, native chains of two to four turns, and, at the start, a typed-
// ahead pair whose Stops never come.
func launchShape(r *rand.Rand) []HookEvent {
	at := orderBase
	var events []HookEvent
	next := 0
	id := func() string { next++; return fmt.Sprintf("shape-%d", next) } // apart from the fixture's turn-1
	prompt := func(ref string) { at++; events = append(events, typedPrompt(ref, at)) }
	stopOf := func(ref string) { at++; events = append(events, stop("launch", ref, at)) }
	if r.IntN(4) == 0 {
		a := id()
		prompt(a)
		prompt(a)
	}
	for range 2 + r.IntN(3) {
		switch r.IntN(3) {
		case 0:
			a := id()
			prompt(a)
			stopOf(a)
		case 1:
			a, b := id(), id()
			prompt(a)
			prompt(a)
			stopOf(a)
			stopOf(b)
		default:
			ref := id()
			prompt(ref)
			for range 1 + r.IntN(3) {
				prompt(ref)
				stopOf(ref)
				ref = id()
			}
			stopOf(ref)
		}
	}
	return events
}

type turnState struct {
	name              string
	opened, finishing int64
	superseded        bool
}

// deliverShape delivers events in order, asking the session before every
// Stop and reading it after, and returns which Stops ran the check on
// their ask, the final turns,
// and which Stops arrived exactly after the events before them in hook
// order.
func deliverShape(t *testing.T, order []HookEvent) (claims map[string]bool, turns []turnState, settled map[string]bool) {
	t.Helper()
	f, reader, writer := turnEndFixture(t, "launch")
	wantOK(t, f.owner(turnEndEvidence("tested"), "evidence"))
	claims, settled = map[string]bool{}, map[string]bool{}
	for i, e := range order {
		if e.Fact == FactTurnFinishing {
			before := true
			for _, d := range order[:i] {
				before = before && d.HookAtMS < e.HookAtMS
			}
			settled[e.EventRef] = before && i == int(e.HookAtMS-orderBase-1)
			ask := f.turnEndAsk(writer, "reader", e.EventRef)
			claims[e.EventRef] = turnEndFor(t, f.hook(reader, e), ask)
			// Each Stop is judged on its own ask: a backlog of unclaimed asks
			// would push it past the notice's ID cap.
			wantOK(t, f.run(reader, Read{IDs: []int64{ask}}, "read-"+e.EventRef))
		} else {
			f.hook(reader, e)
		}
	}
	rows, err := f.db.Query(`SELECT turn_ref, opened_hook_at_ms, COALESCE(finishing_hook_at_ms, 0), superseded FROM turn
		WHERE launch_ref = 'launch' AND opened_hook_at_ms > ? ORDER BY turn_ref`, orderBase)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var s turnState
		if err := rows.Scan(&s.name, &s.opened, &s.finishing, &s.superseded); err != nil {
			t.Fatal(err)
		}
		turns = append(turns, s)
	}
	return claims, turns, settled
}

// #174: in any delivery order (a full shuffle, or one or two callbacks
// arriving late), a Stop that arrives after exactly the events
// before it in hook order runs the check as it does in hook order, and when
// no Stop ran the check that hook order would not, the launch ends with the
// turns of hook order.
func TestTypedAheadTurnsDoNotDependOnDeliveryOrder(t *testing.T) {
	seed := *typedAheadSeed
	if seed == 0 {
		seed = uint64(time.Now().UnixNano())
	}
	r := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	for shape := range 100 {
		events := launchShape(r)
		wantClaims, wantTurns, _ := deliverShape(t, events)
		for run := range 3 {
			order := slices.Clone(events)
			if run == 0 {
				r.Shuffle(len(order), func(i, j int) { order[i], order[j] = order[j], order[i] })
			} else {
				// A late sweep: one or two callbacks arrive after later ones.
				for range 1 + r.IntN(2) {
					from := r.IntN(len(order) - 1)
					to := from + 1 + r.IntN(len(order)-from-1)
					late := order[from]
					order = slices.Insert(slices.Delete(order, from, from+1), to, late)
				}
			}
			claims, turns, settled := deliverShape(t, order)
			where := fmt.Sprintf("seed %d shape %d run %d: delivered %s", seed, shape, run, describe(order))
			extra := false
			for ref, claimed := range claims {
				if settled[ref] && claimed != wantClaims[ref] {
					t.Fatalf("%s: settled Stop %s ran the check %v, hook order %v", where, ref, claimed, wantClaims[ref])
				}
				extra = extra || (claimed && !wantClaims[ref])
			}
			if !extra && !slices.Equal(turns, wantTurns) {
				t.Fatalf("%s:\nturns %+v\nhook order %+v", where, turns, wantTurns)
			}
		}
	}
}
