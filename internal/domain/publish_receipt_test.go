package domain

import (
	"database/sql"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/notice"
)

func (p *publishFixture) observation(id int64, want string) int64 {
	p.t.Helper()
	var got sql.NullString
	var at sql.NullInt64
	if err := p.db.QueryRow(`SELECT observation, observed_at_ms FROM publish_attempt WHERE id = ?`, id).Scan(&got, &at); err != nil {
		p.t.Fatal(err)
	}
	if got.String != want || got.Valid != (want != "") || at.Valid != got.Valid {
		p.t.Fatalf("attempt %d: observation %v at %v, want %q", id, got, at, want)
	}
	return at.Int64
}

func (p *publishFixture) promptReceipt(turn, launch string, atMS int64, ids []int64) HookEvent {
	cmd := prompt(launch, turn, atMS)
	cmd.Receipt = notice.ClaudeReceipt(notice.Claude(ids))
	if cmd.Receipt == "" {
		p.t.Fatal("invalid receipt fixture")
	}
	return cmd
}

func (p *publishFixture) verifyPromptOrigin(cmd HookEvent, want string) {
	p.t.Helper()
	var origin string
	if err := p.db.QueryRow(`SELECT origin FROM observation WHERE principal_id = ? AND launch_ref = ? AND event_ref = ?`,
		p.principalOf(p.reader), cmd.LaunchRef, cmd.EventRef).Scan(&origin); err != nil || origin != want {
		p.t.Fatalf("prompt %s: origin %q (%v), want %q", cmd.EventRef, origin, err, want)
	}
}

func (p *publishFixture) finishPreviousTurn() {
	p.t.Helper()
	at := p.now.UnixMilli()
	p.hook(p.reader, prompt(publishTestLaunch, "previous", at+10))
	p.hook(p.reader, stop(publishTestLaunch, "previous", at+20))
}

func TestPublishReceiptBeforeSendReturnsSettlesOnlyTheLane(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	p.finishPreviousTurn()
	var items []int64
	for i := range OfferCap + 1 {
		items = append(items, p.turnEndAsk(p.writer, "reader", fmt.Sprintf("receipt-%d", i)))
	}
	id := p.reserve()
	ids := p.begin(id)
	if id == 0 || !slices.Equal(ids, items[:OfferCap]) {
		t.Fatal("first batch did not start")
	}
	cmd := p.promptReceipt("notice", publishTestLaunch, p.now.UnixMilli()+100, ids)
	p.hook(p.reader, cmd)
	seen := p.observation(id, "own_turn")
	p.state(id, "submitting")
	p.verifyPromptOrigin(cmd, "crosshall_wake")
	principal := p.principalOf(p.reader)
	if n := p.count(`SELECT COUNT(*) FROM recipient WHERE principal_id = ? AND picked_up_at_ms IS NOT NULL`, principal); n != 0 {
		t.Fatal("a notice observed a pickup")
	}
	if n := p.count(`SELECT COUNT(*) FROM announcement WHERE ref = ? AND via = 'publish'`, id); n != OfferCap {
		t.Fatalf("receipt changed %d retained claims", n)
	}
	p.now = p.now.Add(time.Second)
	p.hook(p.reader, cmd)
	if p.observation(id, "own_turn") != seen ||
		p.count(`SELECT COUNT(*) FROM observation WHERE origin = 'crosshall_wake'`) != 1 {
		t.Fatal("duplicate receipt mutated an existing observation")
	}
	p.resolve(id, PublishSubmitted)
	p.state(id, "submitted")
	if p.observation(id, "own_turn") != seen {
		t.Fatal("transport outcome overwrote observation")
	}
	next := p.reserve()
	if next == 0 || !slices.Equal(p.begin(next), items[OfferCap:]) {
		t.Fatal("matched observation did not admit the next batch")
	}
}

func TestPublishReceiptAbsorbedBeforePromptReplayReturns(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	now := p.now.UnixMilli()
	firstPrompt := prompt(publishTestLaunch, "already-open", now+100)
	p.hook(p.reader, firstPrompt)
	ask := p.turnEndAsk(p.writer, "reader", "absorbed")
	id := p.reserve()
	if id == 0 || !slices.Equal(p.begin(id), []int64{ask}) {
		t.Fatal("reservation did not start")
	}
	receipt := p.promptReceipt(firstPrompt.TurnRef, firstPrompt.LaunchRef, now+200, []int64{ask})
	if hookKey(receipt) != hookKey(firstPrompt) {
		t.Fatal("absorbed prompt did not replay the original operation")
	}
	beforeOps := p.count(`SELECT COUNT(*) FROM op`)
	beforeFacts := p.count(`SELECT COUNT(*) FROM observation`)
	if got := p.hook(p.reader, receipt); !quiet(got) {
		t.Fatalf("replayed receipt changed hook output: %+v", got)
	}
	seen := p.observation(id, "absorbed")
	p.verifyPromptOrigin(firstPrompt, "unknown")
	p.now = p.now.Add(time.Second)
	p.hook(p.reader, receipt)
	if ops, facts := p.count(`SELECT COUNT(*) FROM op`), p.count(`SELECT COUNT(*) FROM observation`); ops != beforeOps || facts != beforeFacts {
		t.Fatalf("receipt replay changed ops %d -> %d, facts %d -> %d", beforeOps, ops, beforeFacts, facts)
	}
	if p.observation(id, "absorbed") != seen {
		t.Fatal("absorbed receipt was recorded twice")
	}
	p.turnEndAsk(p.writer, "reader", "after-absorbed")
	p.resolve(id, PublishSubmitted)
	if p.reserve() != 0 {
		t.Fatal("absorbed route still publishes")
	}
}

func TestPublishReceiptAfterTimeoutAndAfterClear(t *testing.T) {
	for _, clear := range []bool{false, true} {
		t.Run(fmt.Sprint("clear=", clear), func(t *testing.T) {
			p := newPublishFixture(t)
			p.proveRoute()
			p.finishPreviousTurn()
			ask := p.turnEndAsk(p.writer, "reader", "timeout")
			id := p.reserve()
			if id == 0 || !slices.Equal(p.begin(id), []int64{ask}) {
				t.Fatal("reservation did not start")
			}
			p.resolve(id, PublishUncertain)
			p.observation(id, "")
			if clear {
				enrollAndRoute(t, p.fixture, p.reader, "pid-42-clear", "clear", publishTestSocket, "default", p.now.UnixMilli()+100, 1)
				p.proveRoute()
			}
			cmd := p.promptReceipt("late", publishTestLaunch, p.now.UnixMilli()+200, []int64{ask})
			p.hook(p.reader, cmd)
			p.state(id, "uncertain")
			p.turnEndAsk(p.writer, "reader", "after-late")
			if clear {
				crossedClear := p.promptReceipt("crossed-clear", "pid-42-clear", p.now.UnixMilli()+300, []int64{ask})
				p.hook(p.reader, crossedClear)
				p.observation(id, "")
				p.verifyPromptOrigin(cmd, "unknown")
				p.verifyPromptOrigin(crossedClear, "unknown")
				if n := p.count(`SELECT COUNT(*) FROM event WHERE kind = 'publish_receipt_rejected' AND json_extract(detail, '$.reason') = 'stale'`); n != 2 {
					t.Fatalf("late receipts counted = %d, want 2", n)
				}
				if p.reserve() != 0 {
					t.Fatal("old generation's receipt released its physical queue")
				}
			} else {
				p.observation(id, "own_turn")
				p.verifyPromptOrigin(cmd, "crosshall_wake")
				if p.reserve() == 0 {
					t.Fatal("late receipt did not release its queue after timeout")
				}
			}
		})
	}
}

func TestPublishReceiptValidatesListAndAuthenticatedScope(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	ask := p.turnEndAsk(p.writer, "reader", "scope")
	id := p.reserve()
	if id == 0 {
		t.Fatal("no reservation")
	}
	at := p.now.UnixMilli()
	typed := p.promptReceipt("typed-reserved", publishTestLaunch, at+100, []int64{ask})
	p.hook(p.reader, typed)
	p.observation(id, "")
	p.verifyPromptOrigin(typed, "unknown")
	if !slices.Equal(p.begin(id), []int64{ask}) {
		t.Fatal("reservation did not start")
	}

	malformed := prompt(publishTestLaunch, "malformed", at+200)
	malformed.Receipt = fmt.Sprintf("publish:%d %d", ask, ask)
	p.hook(p.reader, malformed)
	p.verifyPromptOrigin(malformed, "unknown")
	if n := p.count(`SELECT COUNT(*) FROM event WHERE kind = 'publish_receipt_rejected' AND json_extract(detail, '$.reason') = 'malformed'`); n != 1 {
		t.Fatal("malformed receipt was not counted")
	}
	mismatch := p.promptReceipt("typed-unmatched", publishTestLaunch, at+300, []int64{ask + 1})
	p.hook(p.reader, mismatch)
	p.verifyPromptOrigin(mismatch, "unknown")
	wrongSession := p.promptReceipt("wrong-session", publishTestLaunch, at+400, []int64{ask})
	wantOK(t, p.exec(p.caller(p.reader), Request{Command: wrongSession, SessionKey: "other-session",
		Vendor: "claude", OpKey: hookKey(wrongSession)}))
	p.verifyPromptOrigin(wrongSession, "unknown")
	p.observation(id, "")
	right := p.promptReceipt("typed-matched", publishTestLaunch, at+500, []int64{ask})
	p.hook(p.reader, right)
	p.observation(id, "unknown")
	p.verifyPromptOrigin(right, "unknown")
	if n := p.count(`SELECT COUNT(*) FROM recipient WHERE message_id = ? AND picked_up_at_ms IS NOT NULL`, ask); n != 0 {
		t.Fatal("typed matching receipt marked a pickup")
	}
}

func TestPublishReceiptWithoutTurnEvidenceStaysUnknown(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	ask := p.turnEndAsk(p.writer, "reader", "missing-prompt")
	id := p.reserve()
	if id == 0 || !slices.Equal(p.begin(id), []int64{ask}) {
		t.Fatal("reservation did not start")
	}
	if n := p.count(`SELECT COUNT(*) FROM turn`); n != 0 {
		t.Fatalf("fixture recorded %d turns before the notice", n)
	}
	cmd := p.promptReceipt("unrecorded-user-turn", publishTestLaunch, p.now.UnixMilli()+100, []int64{ask})
	p.hook(p.reader, cmd)
	p.observation(id, "unknown")
	p.verifyPromptOrigin(cmd, "unknown")
	p.state(id, "submitting")
	if n := p.count(`SELECT COUNT(*) FROM announcement WHERE message_id = ?`, ask); n != 1 {
		t.Fatal("unclassified receipt released the claim")
	}
	p.resolve(id, PublishSubmitted)
	p.turnEndAsk(p.writer, "reader", "after-unknown")
	if p.reserve() != 0 {
		t.Fatal("an unclassified receipt allowed another batch on the same launch")
	}
}

func TestPublishReceiptPriorStopWithLaterActivityStaysUnknown(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	p.finishPreviousTurn()
	at := p.now.UnixMilli()
	const unrecorded = "unrecorded-user-turn"
	p.hook(p.reader, event(publishTestLaunch, FactApprovalRequested, "permission-prompt", unrecorded, at+50, false))
	if n := p.count(`SELECT COUNT(*) FROM observation WHERE kind = 'approval_requested' AND turn_ref = ?`, unrecorded); n != 1 {
		t.Fatal("later activity was not recorded")
	}
	if n := p.count(`SELECT COUNT(*) FROM turn WHERE turn_ref = ?`, unrecorded); n != 0 {
		t.Fatal("fixture recorded the missing prompt")
	}
	ask := p.turnEndAsk(p.writer, "reader", "after-permission-prompt")
	id := p.reserve()
	if id == 0 || !slices.Equal(p.begin(id), []int64{ask}) {
		t.Fatal("reservation did not start")
	}
	cmd := p.promptReceipt(unrecorded, publishTestLaunch, at+100, []int64{ask})
	p.hook(p.reader, cmd)
	p.observation(id, "unknown")
	p.verifyPromptOrigin(cmd, "unknown")
}
