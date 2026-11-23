package domain

import (
	"database/sql"
	"fmt"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/notice"
	"github.com/BeomSeogKim/crosshall/internal/publish"
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
	claude, _ := publish.Lookup("claude")
	line, _ := claude.Notice(ids)
	echoed, _ := claude.Echo(line)
	cmd.Receipt = notice.Receipt(echoed)
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
	// The prompts above opened turns that never got a Stop; a matching
	// receipt with an id of its own still counts as its own turn (#169).
	right := p.promptReceipt("typed-matched", publishTestLaunch, at+500, []int64{ask})
	p.hook(p.reader, right)
	p.observation(id, "own_turn")
	p.verifyPromptOrigin(right, "crosshall_wake")
	if n := p.count(`SELECT COUNT(*) FROM recipient WHERE message_id = ? AND picked_up_at_ms IS NOT NULL`, ask); n != 0 {
		t.Fatal("typed matching receipt marked a pickup")
	}
}

// Owner trial #566: the first prompt of a fresh launch has no previous turn,
// and with nothing observed since the launch started it is the notice's own.
func TestPublishReceiptInAFreshSessionIsItsOwnTurn(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	ask := p.turnEndAsk(p.writer, "reader", "fresh")
	id := p.reserve()
	if id == 0 || !slices.Equal(p.begin(id), []int64{ask}) {
		t.Fatal("reservation did not start")
	}
	if n := p.count(`SELECT COUNT(*) FROM turn`) + p.count(`SELECT COUNT(*) FROM observation WHERE launch_ref = ?`, publishTestLaunch); n != 0 {
		t.Fatalf("fixture recorded %d turns or facts before the notice", n)
	}
	cmd := p.promptReceipt("notice", publishTestLaunch, p.now.UnixMilli()+100, []int64{ask})
	p.hook(p.reader, cmd)
	p.observation(id, "own_turn")
	p.verifyPromptOrigin(cmd, "crosshall_wake")
	p.resolve(id, PublishSubmitted)
	p.turnEndAsk(p.writer, "reader", "after-fresh")
	if p.reserve() == 0 {
		t.Fatal("a fresh session's notice did not admit the next batch")
	}
}

// #169: a turn ended by a denied permission dialog gets no Stop. The notice
// that waited for the dialog then runs with an id of its own, so it is its
// own turn, and the route stays active.
func TestPublishReceiptAfterADeniedTurnIsItsOwnTurn(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	at := p.now.UnixMilli()
	p.hook(p.reader, prompt(publishTestLaunch, "user", at+50))
	p.hook(p.reader, event(publishTestLaunch, FactApprovalRequested, "user:approval", "user", at+60, false))
	ask := p.turnEndAsk(p.writer, "reader", "during the dialog")
	id := p.reserve()
	if id == 0 || !slices.Equal(p.begin(id), []int64{ask}) {
		t.Fatal("reservation did not start")
	}
	cmd := p.promptReceipt("notice", publishTestLaunch, at+100, []int64{ask})
	p.hook(p.reader, cmd)
	p.observation(id, "own_turn")
	p.verifyPromptOrigin(cmd, "crosshall_wake")
	if n := p.count(`SELECT COUNT(*) FROM publish_suspension`); n != 0 {
		t.Fatalf("%d suspensions after a denied turn's notice", n)
	}
	p.resolve(id, PublishSubmitted)
	p.turnEndAsk(p.writer, "reader", "after the notice")
	if p.reserve() == 0 {
		t.Fatal("the route stopped publishing after a denied turn")
	}
}

// A duplicate prompt that a fresh hook process reports for a turn already
// handled opens no turn, so it is no activity that would leave a notice's
// receipt unknown: after a finished turn, and after a turn ended by a denied
// dialog.
func TestPublishReceiptIgnoresADuplicatePrompt(t *testing.T) {
	duplicate := func(ref string, at int64) HookEvent {
		return event(publishTestLaunch, FactPromptSubmitted, ref+":prompt@"+strconv.FormatInt(at, 10), ref, at, true)
	}
	for _, denied := range []bool{false, true} {
		t.Run(map[bool]string{false: "finished", true: "denied"}[denied], func(t *testing.T) {
			p := newPublishFixture(t)
			p.proveRoute()
			p.finishPreviousTurn()
			at := p.now.UnixMilli()
			if denied {
				p.hook(p.reader, prompt(publishTestLaunch, "user", at+50))
				p.hook(p.reader, event(publishTestLaunch, FactApprovalRequested, "user:approval", "user", at+60, false))
			}
			if r := p.hook(p.reader, duplicate("previous", at+70)); !quiet(r) {
				t.Fatalf("the duplicate printed %+v", r)
			}
			ask := p.turnEndAsk(p.writer, "reader", "after the duplicate")
			id := p.reserve()
			if id == 0 || !slices.Equal(p.begin(id), []int64{ask}) {
				t.Fatal("reservation did not start")
			}
			cmd := p.promptReceipt("notice", publishTestLaunch, at+100, []int64{ask})
			p.hook(p.reader, cmd)
			p.observation(id, "own_turn")
			if n := p.count(`SELECT COUNT(*) FROM publish_suspension`); n != 0 {
				t.Fatalf("%d suspensions after a duplicate prompt", n)
			}
		})
	}
}

// A notice that reports the open turn's id was taken into that turn: the
// rule for a turn without a Stop does not apply to it.
func TestPublishReceiptOnTheOpenTurnStaysAbsorbed(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	at := p.now.UnixMilli()
	p.hook(p.reader, prompt(publishTestLaunch, "user", at+50))
	ask := p.turnEndAsk(p.writer, "reader", "busy")
	id := p.reserve()
	if id == 0 || !slices.Equal(p.begin(id), []int64{ask}) {
		t.Fatal("reservation did not start")
	}
	cmd := p.promptReceipt("user", publishTestLaunch, at+100, []int64{ask})
	cmd.EventRef = "user:prompt@" + strconv.FormatInt(at+100, 10)
	p.hook(p.reader, cmd)
	p.observation(id, "absorbed")
}

// Fable #580: a later start of the same launch moves its start time and
// writes no fact, so a compact inside an open user turn must not read fresh.
func TestPublishReceiptAfterACompactInsideAnOpenTurnStaysUnknown(t *testing.T) {
	for _, turn := range []string{"notice", "user"} {
		t.Run("receipt turn "+turn, func(t *testing.T) {
			p := newPublishFixture(t)
			p.proveRoute()
			at := p.now.UnixMilli()
			p.hook(p.reader, prompt(publishTestLaunch, "user", at+50))
			enrollAndRoute(t, p.fixture, p.reader, publishTestLaunch, "compact", publishTestSocket, "default", at+60, 1)
			p.proveRoute()
			ask := p.turnEndAsk(p.writer, "reader", "compact-"+turn)
			id := p.reserve()
			if id == 0 || !slices.Equal(p.begin(id), []int64{ask}) {
				t.Fatal("reservation did not start")
			}
			cmd := p.promptReceipt(turn, publishTestLaunch, at+100, []int64{ask})
			p.hook(p.reader, cmd)
			p.observation(id, "unknown")
		})
	}
}

func TestOpenPlaceholderReceiptsNeverSuspendForTheLaunch(t *testing.T) {
	for _, gaps := range [][]time.Duration{
		{0, 30*time.Minute + 2*time.Second, 51 * time.Minute},
		{0, 60 * time.Minute, 78 * time.Minute},
		{0, 57 * time.Minute},
	} {
		t.Run(fmt.Sprint(gaps), func(t *testing.T) {
			p := newPublishFixture(t)
			p.proveRoute()
			for i, gap := range gaps {
				p.now = p.now.Add(gap)
				at := p.now.UnixMilli()
				turn := fmt.Sprintf("user-%d", i)
				p.hook(p.reader, prompt(publishTestLaunch, turn, at+1))
				p.hook(p.reader, event(publishTestLaunch, FactPromptSubmitted, turn+":repeated", turn, at+2, true))
				p.hook(p.reader, stop(publishTestLaunch, turn, at+3))
				if n := p.count(`SELECT COUNT(*) FROM turn WHERE turn_ref LIKE 'typed-ahead:%' AND finishing_hook_at_ms IS NULL`); n == 0 {
					t.Fatal("no open typed-ahead placeholder before receipt")
				}
				p.turnEndAsk(p.writer, "reader", "open-placeholder-"+turn)
				id := p.reserve()
				if id == 0 {
					t.Fatal("the previous suspension did not lift")
				}
				ids := p.begin(id)
				p.resolve(id, PublishSubmitted)
				p.hook(p.reader, p.promptReceipt("notice-"+turn, publishTestLaunch, at+4, ids))
				p.observation(id, "unknown")
				if s := p.publishState(); s.State != "suspended" || s.Notice != id || s.UntilMS != p.now.Add(30*time.Minute).UnixMilli() {
					t.Fatalf("receipt %d: %+v, want a 30-minute suspension", i+1, s)
				}
			}
			p.now = p.now.Add(30 * time.Minute)
			p.turnEndAsk(p.writer, "reader", "after-replay")
			p.requirePublish("the final suspension expired", "available", "")
		})
	}
}

// Fable #601: an auto-compact inside a long turn; the turn that opened before
// that start and finished after it is the launch's last turn.
func TestPublishReceiptAfterACompactedTurnFinishedIsItsOwnTurn(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	at := p.now.UnixMilli()
	p.hook(p.reader, prompt(publishTestLaunch, "user", at+50))
	enrollAndRoute(t, p.fixture, p.reader, publishTestLaunch, "compact", publishTestSocket, "default", at+60, 1)
	p.proveRoute()
	p.hook(p.reader, stop(publishTestLaunch, "user", at+70))
	ask := p.turnEndAsk(p.writer, "reader", "after-compacted-turn")
	id := p.reserve()
	if id == 0 || !slices.Equal(p.begin(id), []int64{ask}) {
		t.Fatal("reservation did not start")
	}
	cmd := p.promptReceipt("notice", publishTestLaunch, at+100, []int64{ask})
	p.hook(p.reader, cmd)
	p.observation(id, "own_turn")
	p.verifyPromptOrigin(cmd, "crosshall_wake")
}

func TestPublishReceiptAfterAClearOfAnIdleSessionIsItsOwnTurn(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	p.finishPreviousTurn()
	at := p.now.UnixMilli()
	enrollAndRoute(t, p.fixture, p.reader, publishTestLaunch, "clear", publishTestSocket, "default", at+60, 1)
	p.proveRoute()
	ask := p.turnEndAsk(p.writer, "reader", "after-clear")
	id := p.reserve()
	if id == 0 || !slices.Equal(p.begin(id), []int64{ask}) {
		t.Fatal("reservation did not start")
	}
	cmd := p.promptReceipt("notice", publishTestLaunch, at+100, []int64{ask})
	p.hook(p.reader, cmd)
	p.observation(id, "own_turn")
	p.verifyPromptOrigin(cmd, "crosshall_wake")
}

// A fresh launch whose user prompt was dropped is not idle once anything of
// that turn was seen.
func TestPublishReceiptWithoutTurnEvidenceStaysUnknown(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	const unrecorded = "unrecorded-user-turn"
	p.hook(p.reader, event(publishTestLaunch, FactApprovalRequested, "permission-prompt", unrecorded, p.now.UnixMilli()+50, false))
	ask := p.turnEndAsk(p.writer, "reader", "missing-prompt")
	id := p.reserve()
	if id == 0 || !slices.Equal(p.begin(id), []int64{ask}) {
		t.Fatal("reservation did not start")
	}
	if n := p.count(`SELECT COUNT(*) FROM turn`); n != 0 {
		t.Fatalf("fixture recorded %d turns before the notice", n)
	}
	cmd := p.promptReceipt(unrecorded, publishTestLaunch, p.now.UnixMilli()+100, []int64{ask})
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

// A prior turn without a Stop is not taken as ended when another turn's
// activity was observed since it opened: here the next prompt's callback was
// lost and only its approval arrived, so a receipt with that turn's id
// stays unknown.
func TestPublishReceiptAfterAnUnfinishedTurnWithLaterActivityStaysUnknown(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	at := p.now.UnixMilli()
	p.hook(p.reader, prompt(publishTestLaunch, "prior", at+10)) // no Stop
	const unrecorded = "unrecorded-user-turn"
	p.hook(p.reader, event(publishTestLaunch, FactApprovalRequested, "permission-prompt", unrecorded, at+50, false))
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

// Fable: a publish notice the session observed counts as already told for
// the prompt line, like a turn-end announcement; a notice claimed but not
// observed does not, since the session may never have seen it.
func TestThePromptLineSkipsIDsAnObservedNoticeCarried(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	p.finishPreviousTurn()
	seen := p.turnEndAsk(p.writer, "reader", "seen")
	id := p.reserve()
	if ids := p.begin(id); !slices.Equal(ids, []int64{seen}) {
		t.Fatalf("notice carried %v", ids)
	}
	got := p.hook(p.reader, p.promptReceipt("notice", publishTestLaunch, p.now.UnixMilli()+100, []int64{seen}))
	p.observation(id, "own_turn")
	if got.PendingLine != "" {
		t.Fatalf("the notice's own turn repeats its IDs: %q", got.PendingLine)
	}
	p.hook(p.reader, stop(publishTestLaunch, "notice", p.now.UnixMilli()+200))
	unseen := p.turnEndAsk(p.writer, "reader", "unseen")
	p.resolve(id, PublishSubmitted)
	id = p.reserve()
	if ids := p.begin(id); !slices.Equal(ids, []int64{unseen}) {
		t.Fatalf("second notice carried %v", ids)
	}
	got = p.hook(p.reader, prompt(publishTestLaunch, "user", p.now.UnixMilli()+300))
	if got.PendingLine != pendingLine(unseen) {
		t.Fatalf("prompt line %q, want only the unobserved notice's %d", got.PendingLine, unseen)
	}
}

// Fable #448: a notice a gate blocked never reached the model, so its IDs
// stay on the prompt line.
func TestThePromptLineKeepsIDsOfABlockedNotice(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	p.finishPreviousTurn()
	blocked := p.turnEndAsk(p.writer, "reader", "blocked")
	id := p.reserve()
	if ids := p.begin(id); !slices.Equal(ids, []int64{blocked}) {
		t.Fatalf("notice carried %v", ids)
	}
	p.mutate(`UPDATE publish_attempt SET observation = 'blocked', observed_at_ms = ? WHERE id = ?`, p.now.UnixMilli(), id)
	if got := p.hook(p.reader, prompt(publishTestLaunch, "user", p.now.UnixMilli()+300)); got.PendingLine != pendingLine(blocked) {
		t.Fatalf("prompt line %q, want the blocked notice's %d", got.PendingLine, blocked)
	}
}
