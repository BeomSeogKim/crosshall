package domain

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

func (p *publishFixture) reserveResume() (int64, bool) {
	p.t.Helper()
	var id int64
	var owes bool
	p.update(func(ctx context.Context, tx store.Tx) (err error) {
		id, owes, err = ReserveResume(ctx, tx, p.binding.ID, p.now)
		return err
	})
	return id, owes
}

func (p *publishFixture) beginResume(id int64) []int64 {
	p.t.Helper()
	var ids []int64
	p.update(func(ctx context.Context, tx store.Tx) (err error) {
		_, ids, err = BeginResume(ctx, tx, id, p.now)
		return err
	})
	return ids
}

// A member that read a request and still owes it gets a resume attempt
// naming it, with no announcement; the regular fence would release it as
// stale, the resume fence submits it.
func TestResumeCarriesReadOwedRequests(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	p.finishPreviousTurn()
	p.pickUp(p.reader, p.beforeID)
	if id := p.reserve(); id != 0 {
		t.Fatalf("a regular notice carried a read request: attempt %d", id)
	}

	id, owes := p.reserveResume()
	if id == 0 || !owes {
		t.Fatalf("resume reserved %d, owes %v", id, owes)
	}
	if items := inTx(t, p.fixture, func(ctx context.Context, tx store.Tx) ([]int64, error) { return publishAttemptIDs(ctx, tx, id) }); !slices.Equal(items, []int64{p.beforeID}) {
		t.Fatalf("resume items %v, want [%d]", items, p.beforeID)
	}
	if p.count(`SELECT COUNT(*) FROM announcement WHERE ref = ?`, id) != 0 ||
		p.count(`SELECT COUNT(*) FROM event WHERE kind = 'publish_resume'`) != 1 {
		t.Fatal("a resume wrote an announcement, or no owner event")
	}
	if again, _ := p.reserveResume(); again != 0 {
		t.Fatal("a second resume was reserved beside the first")
	}

	// The regular fence reads a read item as stale: the safe side.
	if ids := p.begin(id); len(ids) != 0 {
		t.Fatalf("the regular fence submitted a resume: %v", ids)
	}
	p.state(id, "not_submitted")

	id, _ = p.reserveResume()
	if ids := p.beginResume(id); !slices.Equal(ids, []int64{p.beforeID}) {
		t.Fatalf("resume fence gave %v", ids)
	}
	p.state(id, "submitting")
	// who reads a resume in flight as any notice in flight.
	w := inTx(t, p.fixture, func(ctx context.Context, tx store.Tx) (Wake, error) { return memberWake(ctx, tx, p.binding.ID, p.now) })
	if w.Why != WhyHeld {
		t.Fatalf("a member with a resume in flight reads %+v", w)
	}
}

// A resume is released when its request was answered before it began, and
// none is reserved for a member that owes nothing.
func TestResumeReleasedWhenTheRequestIsAnswered(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	p.finishPreviousTurn()
	p.pickUp(p.reader, p.beforeID)
	id, _ := p.reserveResume()
	wantOK(t, p.reply(p.reader, Reply{To: p.beforeID, Body: "done"}, "answer"))
	if ids := p.beginResume(id); len(ids) != 0 {
		t.Fatalf("a resume of an answered request began: %v", ids)
	}
	if id, owes := p.reserveResume(); id != 0 || owes {
		t.Fatalf("a member that owes nothing: resume %d, owes %v", id, owes)
	}
}

// A member in a running turn gets its resume after the turn. Unread owed
// work a notice will carry is the notice's; unread work stored before the
// route is the resume's, since no notice carries it.
func TestResumeWaitsForTheTurnAndLeavesNoticedWork(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	p.finishPreviousTurn()
	fresh := p.turnEndAsk(p.writer, "reader", "after-route")
	id, _ := p.reserveResume()
	items := inTx(t, p.fixture, func(ctx context.Context, tx store.Tx) ([]int64, error) { return publishAttemptIDs(ctx, tx, id) })
	if !slices.Equal(items, []int64{p.beforeID}) {
		t.Fatalf("resume items %v: want only %d, stored before the route (%d is a notice's)", items, p.beforeID, fresh)
	}
	p.beginResume(id)
	p.resolve(id, PublishNotSubmitted)

	p.hook(p.reader, prompt(publishTestLaunch, "busy", p.now.UnixMilli()+10))
	if id, owes := p.reserveResume(); id != 0 || !owes {
		t.Fatalf("during a turn: resume %d, owes %v", id, owes)
	}
	p.hook(p.reader, stop(publishTestLaunch, "busy", p.now.UnixMilli()+20))
	if id, _ := p.reserveResume(); id == 0 {
		t.Fatal("no resume after the turn ended")
	}
}

// The owner's start reads who owes, and whether the hall can wake each.
func TestOwingMembersSayWhoCanBeWoken(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	p.pickUp(p.reader, p.beforeID)
	codex, _ := codexMember(t, p.fixture, "far")
	p.turnEndAsk(p.writer, "far", "to-codex")
	members := inTx(t, p.fixture, func(ctx context.Context, tx store.Tx) ([]OwingMember, error) { return OwingMembers(ctx, tx, p.now) })
	if len(members) != 2 || members[0].Alias != "far" || members[0].CanResume() || members[1].Alias != "reader" || !members[1].CanResume() ||
		members[1].Owed != 1 {
		t.Fatalf("owing members %+v", members)
	}
	_ = codex
	r := decode[OwnerOwedResult](t, p.owner(OwnerOwed{}, ""))
	if len(r.Members) != 2 {
		t.Fatalf("owner owed %+v", r)
	}
}

// M3: a member's Stop was lost while the dispatcher was stopped, so its
// last fact is a prompt. That prompt stops counting 30 minutes after its
// hook time, inside the resume window, and the member gets its resume.
func TestResumeReachesAMemberWhoseStopWasLost(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	p.pickUp(p.reader, p.beforeID)
	p.hook(p.reader, prompt(publishTestLaunch, "lost", p.now.UnixMilli()+10))
	if id, _ := p.reserveResume(); id != 0 {
		t.Fatal("a resume during the reported turn")
	}
	p.now = p.now.Add(time.Duration(factCapMS[FactPromptSubmitted])*time.Millisecond + time.Second)
	if time.Duration(factCapMS[FactPromptSubmitted])*time.Millisecond >= ResumeWindow {
		t.Fatal("the prompt's cap does not end inside the resume window")
	}
	p.proveRoute()
	if id, _ := p.reserveResume(); id == 0 {
		t.Fatal("no resume once the lost Stop's prompt stopped counting")
	}
}

// The resume pass runs every publisher tick for each due member, inside
// the writer transaction, for up to an hour: its owed lookup starts from
// the member's open obligations and never scans a growing table.
func TestResumeLookupStaysASearch(t *testing.T) {
	p := newPublishFixture(t)
	rows, err := p.db.Query("EXPLAIN QUERY PLAN SELECT EXISTS (SELECT 1 "+owedResumeItems+")", nil, p.binding.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	joined := strings.Join(plan, " | ")
	for _, step := range plan {
		if strings.HasPrefix(step, "SCAN ") && step != "SCAN CONSTANT ROW" {
			t.Fatalf("plan scans a table: %s", joined)
		}
	}
	if !strings.Contains(joined, "obligation_open") {
		t.Fatalf("plan does not start from the open obligations: %s", joined)
	}
}
