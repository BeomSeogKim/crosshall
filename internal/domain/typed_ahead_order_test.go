package domain

import (
	"fmt"
	"strconv"
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

// #174: a typed-ahead turn's Stop delivered before its predecessor's Stop
// still ends the typed-ahead turn once that Stop arrives, so no placeholder
// is left to take the next turn's Stop.
func TestAnEarlyTypedAheadStopLeavesNoPlaceholder(t *testing.T) {
	f, reader, writer := turnEndFixture(t, "launch")
	wantOK(t, f.owner(turnEndEvidence("tested"), "evidence"))
	for _, e := range []HookEvent{
		typedPrompt("turn-a", orderBase+1), typedPrompt("turn-a", orderBase+2),
		stop("launch", "turn-b", orderBase+4), stop("launch", "turn-a", orderBase+3),
		typedPrompt("turn-c", orderBase+5),
	} {
		f.hook(reader, e)
	}
	ask := f.turnEndAsk(writer, "reader", "during c")
	if r := f.hook(reader, stop("launch", "turn-c", orderBase+6)); !turnEndFor(t, r, ask) {
		t.Fatalf("Stop C: %q, want the check to run", r.TurnEndLine)
	}
	if n := f.count(`SELECT COUNT(*) FROM turn WHERE turn_ref LIKE 'typed-ahead:%'`); n != 0 {
		t.Fatalf("%d placeholders left", n)
	}
}

// #174: an early Stop of a later turn in a chain does not take an earlier
// placeholder once the prompts before it arrive.
func TestAnEarlyChainStopTakesItsOwnTurn(t *testing.T) {
	f, reader, writer := turnEndFixture(t, "launch")
	wantOK(t, f.owner(turnEndEvidence("tested"), "evidence"))
	for _, e := range []HookEvent{
		typedPrompt("turn-a", orderBase+1), typedPrompt("turn-a", orderBase+2), stop("launch", "turn-a", orderBase+3),
		stop("launch", "turn-c", orderBase+7), typedPrompt("turn-b", orderBase+4), stop("launch", "turn-b", orderBase+5),
		typedPrompt("turn-c", orderBase+6),
	} {
		f.hook(reader, e)
	}
	ask := f.turnEndAsk(writer, "reader", "during d")
	if r := f.hook(reader, stop("launch", "turn-d", orderBase+8)); !turnEndFor(t, r, ask) {
		t.Fatalf("Stop D: %q, want the check to run", r.TurnEndLine)
	}
}

// #174: a turn that already gave its turn-end notice keeps its name when a
// late prompt places it differently: the original prompt of a typed-ahead
// pair arriving after the pair's turn was checked, and the prompt that
// typed a chain's turn ahead arriving after that turn was checked.
func TestACheckedTurnKeepsItsNameWhenALatePromptMovesIt(t *testing.T) {
	t.Run("original prompt", func(t *testing.T) {
		f, reader, writer := turnEndFixture(t, "launch")
		wantOK(t, f.owner(turnEndEvidence("tested"), "evidence"))
		f.hook(reader, typedPrompt("turn-a", orderBase+2))
		first := f.turnEndAsk(writer, "reader", "during a")
		if r := f.hook(reader, stop("launch", "turn-a", orderBase+3)); !turnEndFor(t, r, first) {
			t.Fatalf("Stop A: %q, want the check to run", r.TurnEndLine)
		}
		f.hook(reader, typedPrompt("turn-a", orderBase+1))
		if n := f.count(`SELECT COUNT(*) FROM turn t JOIN turn_end_notice n USING (principal_id, launch_ref, turn_ref)
			WHERE t.turn_ref = 'turn-a' AND t.opened_hook_at_ms = ` + strconv.FormatInt(orderBase+1, 10)); n != 1 {
			t.Fatal("the checked turn was not kept and moved to its original prompt")
		}
		ask := f.turnEndAsk(writer, "reader", "during b")
		if r := f.hook(reader, stop("launch", "turn-b", orderBase+4)); !turnEndFor(t, r, ask) {
			t.Fatalf("Stop B: %q, want the check to run", r.TurnEndLine)
		}
	})
	t.Run("chain", func(t *testing.T) {
		f, reader, writer := turnEndFixture(t, "launch")
		wantOK(t, f.owner(turnEndEvidence("tested"), "evidence"))
		f.hook(reader, typedPrompt("turn-a", orderBase+1))
		f.hook(reader, stop("launch", "turn-a", orderBase+3))
		f.hook(reader, typedPrompt("turn-b", orderBase+4))
		first := f.turnEndAsk(writer, "reader", "during b")
		if r := f.hook(reader, stop("launch", "turn-b", orderBase+5)); !turnEndFor(t, r, first) {
			t.Fatalf("Stop B: %q, want the check to run", r.TurnEndLine)
		}
		f.hook(reader, typedPrompt("turn-a", orderBase+2))
		if n := f.count(`SELECT COUNT(*) FROM turn_end_notice WHERE turn_ref = 'turn-b'`); n != 1 {
			t.Fatal("the checked turn lost its notice")
		}
		ask := f.turnEndAsk(writer, "reader", "during c")
		if r := f.hook(reader, stop("launch", "turn-c", orderBase+6)); !turnEndFor(t, r, ask) {
			t.Fatalf("Stop C: %q, want the check to run", r.TurnEndLine)
		}
	})
}

// #174: a placeholder that never ends (its Stops were lost) does not keep a
// later chain from being placed when its Stop arrives late.
func TestAnUnendedEarlierPlaceholderDoesNotBlockALaterChain(t *testing.T) {
	f, reader, writer := turnEndFixture(t, "launch")
	wantOK(t, f.owner(turnEndEvidence("tested"), "evidence"))
	for _, e := range []HookEvent{
		typedPrompt("turn-a", orderBase+1), typedPrompt("turn-a", orderBase+2),
		typedPrompt("turn-x", orderBase+10), stop("launch", "turn-x", orderBase+11),
		typedPrompt("turn-d", orderBase+20), typedPrompt("turn-d", orderBase+21),
		typedPrompt("turn-e", orderBase+23), stop("launch", "turn-d", orderBase+22),
	} {
		f.hook(reader, e)
	}
	f.hook(reader, stop("launch", "turn-e", orderBase+24)) // F was typed during E
	ask := f.turnEndAsk(writer, "reader", "during f")
	if r := f.hook(reader, stop("launch", "turn-f", orderBase+25)); !turnEndFor(t, r, ask) {
		t.Fatalf("Stop F: %q, want the check to run", r.TurnEndLine)
	}
}

// A duplicate of the latest prompt that comes after its turn's Stop is a
// replay, not a prompt typed ahead: the turn was no longer running.
func TestADuplicateAfterTheLatestTurnEndedIsAReplay(t *testing.T) {
	f, reader, _ := turnEndFixture(t, "launch")
	wantOK(t, f.owner(turnEndEvidence("tested"), "evidence"))
	f.hook(reader, typedPrompt("turn-a", orderBase+1))
	f.hook(reader, stop("launch", "turn-a", orderBase+2))
	turns := f.count(`SELECT COUNT(*) FROM turn`)
	if r := f.hook(reader, typedPrompt("turn-a", orderBase+3)); !quiet(r) {
		t.Fatalf("the duplicate printed %+v", r)
	}
	if n := f.count(`SELECT COUNT(*) FROM turn`); n != turns {
		t.Fatalf("the duplicate opened %d turns", n-turns)
	}
}

// #174: a Stop recorded in the same millisecond as its turn's first prompt
// comes after that prompt, whichever arrives first: the prompt binds the
// chain's placeholder and opens the next one, which the next Stop takes.
func TestAStopInItsPromptsMillisecondFollowsIt(t *testing.T) {
	for _, stopFirst := range []bool{false, true} {
		f, reader, writer := turnEndFixture(t, "launch")
		wantOK(t, f.owner(turnEndEvidence("tested"), "evidence"))
		f.hook(reader, typedPrompt("turn-z", orderBase+1))
		f.hook(reader, typedPrompt("turn-z", orderBase+2))
		f.hook(reader, stop("launch", "turn-z", orderBase+3))
		same := []HookEvent{typedPrompt("turn-a", orderBase+5), stop("launch", "turn-a", orderBase+5)}
		if stopFirst {
			same[0], same[1] = same[1], same[0]
		}
		for _, e := range same {
			f.hook(reader, e)
		}
		ask := f.turnEndAsk(writer, "reader", "during c")
		if r := f.hook(reader, stop("launch", "turn-c", orderBase+6)); !turnEndFor(t, r, ask) {
			t.Fatalf("stop delivered first %v: Stop C %q, want the check to run", stopFirst, r.TurnEndLine)
		}
	}
}
