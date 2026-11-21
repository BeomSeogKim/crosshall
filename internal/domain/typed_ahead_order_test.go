package domain

import (
	"fmt"
	"testing"
)

// permutations lists every delivery order of events.
func permutations(events []HookEvent) [][]HookEvent {
	if len(events) <= 1 {
		return [][]HookEvent{append([]HookEvent(nil), events...)}
	}
	var out [][]HookEvent
	for i := range events {
		rest := append(append([]HookEvent(nil), events[:i]...), events[i+1:]...)
		for _, tail := range permutations(rest) {
			out = append(out, append([]HookEvent{events[i]}, tail...))
		}
	}
	return out
}

func describe(order []HookEvent) string {
	s := ""
	for _, e := range order {
		kind := "Prompt"
		if e.Fact == FactTurnFinishing {
			kind = "Stop"
		}
		s += fmt.Sprintf("%s %s@%d, ", kind, e.TurnRef, e.HookAtMS%100)
	}
	return s
}

// deliveredInAnyOrder delivers events in every order, then a request and the
// final Stop, and wants the final Stop's turn-end check to run each time.
func deliveredInAnyOrder(t *testing.T, events []HookEvent, final HookEvent) {
	t.Helper()
	for _, order := range permutations(events) {
		f, reader, writer := turnEndFixture(t, "launch")
		wantOK(t, f.owner(turnEndEvidence("tested"), "evidence"))
		for _, e := range order {
			f.hook(reader, e)
		}
		ask := f.turnEndAsk(writer, "reader", "during the last turn")
		if r := f.hook(reader, final); !turnEndFor(t, r, ask) {
			t.Errorf("delivered %s then %s: end line %q, want the check to run", describe(order), describe([]HookEvent{final}), r.TurnEndLine)
		}
	}
}

// orderBase follows the fixture's own first turn.
const orderBase = int64(1_800_000_000_010)

// #174: B typed while A runs; its turn is checked at its Stop in any order
// the earlier callbacks are delivered in (including the typed-ahead prompt
// after A's Stop).
func TestATypedAheadTurnIsCheckedInAnyDeliveryOrder(t *testing.T) {
	deliveredInAnyOrder(t, []HookEvent{
		typedPrompt("turn-a", orderBase+1), typedPrompt("turn-a", orderBase+2), stop("launch", "turn-a", orderBase+3),
	}, stop("launch", "turn-b", orderBase+4))
}

// #174: the native chain (B typed during A, C during B) gives C its check in
// any order the earlier callbacks are delivered in.
func TestATypedAheadChainIsCheckedInAnyDeliveryOrder(t *testing.T) {
	deliveredInAnyOrder(t, []HookEvent{
		typedPrompt("turn-a", orderBase+1), typedPrompt("turn-a", orderBase+2), stop("launch", "turn-a", orderBase+3),
		typedPrompt("turn-b", orderBase+4), stop("launch", "turn-b", orderBase+5),
	}, stop("launch", "turn-c", orderBase+6))
}

// #174: an unknown Stop whose hook time is before A's Stop, delivered after
// it, was not the typed-ahead turn's end: it adopts nothing, and the
// typed-ahead turn's own Stop is still checked.
func TestAnUnknownStopBeforeItsPredecessorsStopAdoptsNothingWhenDeliveredLate(t *testing.T) {
	f, reader, writer := turnEndFixture(t, "launch")
	wantOK(t, f.owner(turnEndEvidence("tested"), "evidence"))
	f.hook(reader, typedPrompt("turn-a", orderBase+1))
	f.hook(reader, typedPrompt("turn-a", orderBase+2))
	f.hook(reader, stop("launch", "turn-a", orderBase+4))
	if r := f.hook(reader, stop("launch", "turn-x", orderBase+3)); r.TurnEndLine != "" {
		t.Fatalf("an unknown earlier Stop ran the check: %q", r.TurnEndLine)
	}
	if n := f.count(`SELECT COUNT(*) FROM turn WHERE turn_ref = 'turn-x'`); n != 0 {
		t.Fatal("an unknown earlier Stop adopted the typed-ahead turn")
	}
	ask := f.turnEndAsk(writer, "reader", "during b")
	if r := f.hook(reader, stop("launch", "turn-b", orderBase+5)); !turnEndFor(t, r, ask) {
		t.Fatalf("the typed-ahead turn's own Stop: %q, want the check to run", r.TurnEndLine)
	}
}
