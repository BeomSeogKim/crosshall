package domain

import (
	"strconv"
	"strings"
	"testing"
)

// #163: a prompt typed while the previous turn runs reports that turn's id,
// and its Stop carries an id no prompt had. It is its own turn: it gets its
// pending line, and the turn-end check runs at its end.
func TestATypedAheadPromptIsItsOwnTurn(t *testing.T) {
	f, reader, writer := turnEndFixture(t, "launch")
	wantOK(t, f.owner(turnEndEvidence("tested"), "evidence"))
	at := f.now.UnixMilli() + 1
	first := f.turnEndAsk(writer, "reader", "first")
	if r := f.hook(reader, prompt("launch", "turn-a", at)); !strings.Contains(r.PendingLine, strconv.FormatInt(first, 10)) {
		t.Fatalf("first prompt pending line %q", r.PendingLine)
	}
	second := f.turnEndAsk(writer, "reader", "second")
	typedAhead := event("launch", FactPromptSubmitted, "turn-a:prompt@"+strconv.FormatInt(at+1, 10), "turn-a", at+1, true)
	if r := f.hook(reader, typedAhead); !strings.Contains(r.PendingLine, strconv.FormatInt(second, 10)) {
		t.Fatalf("typed-ahead prompt pending line %q, want request %d offered", r.PendingLine, second)
	}
	if r := f.hook(reader, typedAhead); !quiet(r) {
		t.Fatalf("a retry of the same callback: %+v, want nothing", r)
	}
	if r := f.hook(reader, stop("launch", "turn-a", at+2)); r.TurnEndLine != "" {
		t.Fatalf("the first turn, with a prompt queued behind it, ran the check: %q", r.TurnEndLine)
	}
	r := f.hook(reader, stop("launch", "turn-b", at+3))
	if !strings.Contains(r.TurnEndLine, strconv.FormatInt(second, 10)) {
		t.Fatalf("the typed-ahead turn's end line %q, want the check to run", r.TurnEndLine)
	}
	if n := f.count(`SELECT COUNT(*) FROM turn WHERE turn_ref LIKE 'typed-ahead:%'`); n != 0 {
		t.Fatalf("%d placeholder turns left after the turn end adopted it", n)
	}
}

// A turn end with an unknown id and no typed-ahead prompt before it opens
// nothing, as before.
func TestAnUnknownTurnEndWithoutATypedAheadPromptStaysUnclaimed(t *testing.T) {
	f, reader, writer := turnEndFixture(t, "launch")
	wantOK(t, f.owner(turnEndEvidence("tested"), "evidence"))
	at := f.now.UnixMilli() + 1
	f.turnEndAsk(writer, "reader", "open")
	f.hook(reader, prompt("launch", "turn-a", at))
	f.hook(reader, stop("launch", "turn-a", at+1))
	if r := f.hook(reader, stop("launch", "turn-x", at+2)); r.TurnEndLine != "" {
		t.Fatalf("an unknown turn end ran the check: %q", r.TurnEndLine)
	}
	if n := f.count(`SELECT COUNT(*) FROM turn WHERE turn_ref = 'turn-x'`); n != 0 {
		t.Fatal("an unknown turn end opened a turn")
	}
}

// #163: a publish notice does not take the typed-ahead shape in Claude Code
// (it runs after the typed turn's Stop with an id of its own). Should a
// receipt ever arrive on an open turn's id, it stays absorbed, which
// suspends the route as before; its callback, now told apart from the first
// by the hook's entry time, opens a typed-ahead turn.
func TestAReceiptOnARepeatedTurnIdStaysAbsorbed(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	now := p.now.UnixMilli()
	firstPrompt := prompt(publishTestLaunch, "already-open", now+100)
	p.hook(p.reader, firstPrompt)
	ask := p.turnEndAsk(p.writer, "reader", "queued")
	id := p.reserve()
	if id == 0 {
		t.Fatal("reservation did not start")
	}
	p.begin(id)
	receipt := p.promptReceipt(firstPrompt.TurnRef, firstPrompt.LaunchRef, now+200, []int64{ask})
	receipt.EventRef = firstPrompt.EventRef + "@" + strconv.FormatInt(now+200, 10)
	p.hook(p.reader, receipt)
	p.observation(id, "absorbed")
	if n := p.count(`SELECT COUNT(*) FROM turn WHERE turn_ref LIKE 'typed-ahead:%'`); n != 1 {
		t.Fatalf("typed-ahead turns %d, want the notice's own", n)
	}
	p.resolve(id, PublishSubmitted)
	if p.reserve() != 0 {
		t.Fatal("the route still publishes after an absorbed receipt")
	}
}
