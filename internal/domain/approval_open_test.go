package domain

import (
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/publish"
)

// approvalFact reports an approval request in a new turn of the publish
// fixture's reader.
func (p *publishFixture) approvalFact(turn string) {
	p.t.Helper()
	at := p.now.UnixMilli()
	p.hook(p.reader, prompt(publishTestLaunch, turn, at))
	p.hook(p.reader, event(publishTestLaunch, FactApprovalRequested, turn+":approval", turn, at+1, false))
}

// readerApproval is what a sender, the owner's who and the reader's latest
// fact say about the reader now.
func (p *publishFixture) readerApproval(key string) (send Delivery, ownerOpen, factOpen bool) {
	p.t.Helper()
	r := p.ask(p.writer, Ask{To: []string{"reader"}, New: true, Body: "work"}, key)
	sent := decode[AskResult](p.t, r)
	if len(sent.Recipients) != 1 {
		p.t.Fatalf("%s: ask %s %s", key, r.Code, r.Data)
	}
	send = sent.Recipients[0]
	ownerOpen, factOpen = p.ownerAndFact()
	return send, ownerOpen, factOpen
}

// ownerAndFact says whether the owner's who and the reader's latest fact
// show an open approval.
func (p *publishFixture) ownerAndFact() (ownerOpen, factOpen bool) {
	p.t.Helper()
	for _, ch := range decode[OwnerWhoResult](p.t, p.owner(OwnerWho{}, "")).Channels {
		for _, m := range ch.Members {
			if m.Alias == "reader" && m.Observed != nil {
				ownerOpen = m.Observed.ApprovalOpen
			}
		}
	}
	fact, err := latestFact(p.t.Context(), p.db, p.principalOf(p.reader))
	if err != nil {
		p.t.Fatal(err)
	}
	return ownerOpen, fact != nil && fact.ApprovalOpen
}

// A turn blocked on an approval moves only when the
// member's owner answers. The sender is told so instead of "notice queued",
// the owner sees it, and it does not age out at the fact's 30-minute cap.
func TestAnOpenApprovalWaitsOnTheOwnerUntilSomethingEndsIt(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	waitsOnOwner := func(name, key string, want bool) {
		t.Helper()
		send, owner, fact := p.readerApproval(key)
		if got := send.Wake.Why == WhyApproval; got != want || owner != want || fact != want {
			t.Errorf("%s: send %+v (next %q), owner who %v, fact %v; want waits on its owner %v", name, send.Wake, send.Next, owner, fact, want)
		}
		if want && (send.Next != "waits on its owner's approval" || send.Wake.NeedsManualWake()) {
			t.Errorf("%s: next %q, needs a manual wake %v", name, send.Next, send.Wake.NeedsManualWake())
		}
	}

	waitsOnOwner("no approval", "before", false)
	p.approvalFact("turn-1")
	waitsOnOwner("approval requested", "open", true)
	p.now = p.now.Add(31 * time.Minute)
	waitsOnOwner("past the fact's cap", "aged", true)
	// Nor does the hall send a notice into the blocked turn once the cap passed.
	if id := p.reserve(); id != 0 {
		t.Fatalf("a notice was reserved into a turn that waits on its owner: attempt %d", id)
	}
	// The member ran a crosshall command: the dialog was answered.
	wantOK(t, p.run(p.reader, Who{}, ""))
	waitsOnOwner("after a command of the member", "command", false)

	p.approvalFact("turn-2")
	waitsOnOwner("a second approval", "second", true)
	p.hook(p.reader, stop(publishTestLaunch, "turn-2", p.now.UnixMilli()+2))
	waitsOnOwner("after the turn's Stop", "stopped", false)
}

// An approval must not outlive its session: a launch known dead, without a
// session end, ends it, and so does the owner's depart.
func TestAnOpenApprovalEndsWithItsLaunch(t *testing.T) {
	t.Run("process gone", func(t *testing.T) {
		p := newPublishFixture(t)
		p.proveRoute()
		p.approvalFact("turn-1")
		if _, _, open := p.readerApproval("open"); !open {
			t.Fatal("fixture: the approval is not open")
		}
		launches := liveClaudeLaunches(p.fixture, 0, 5)
		if len(launches) != 1 {
			t.Fatalf("fixture launches: %+v", launches)
		}
		p.now = p.now.Add(2 * time.Second)
		if !applyClaudeProbes(p.fixture, ClaudeLaunchProbe{ClaudeLaunch: launches[0], Gone: true}) {
			t.Fatal("the probe did not end the launch")
		}
		// The probe departs the member, so no ask reaches it; neither its
		// fact nor the owner's who may still say it waits on the owner.
		if owner, fact := p.ownerAndFact(); owner || fact {
			t.Fatalf("after its process was gone: owner who %v, fact %v", owner, fact)
		}
	})
	t.Run("owner depart", func(t *testing.T) {
		p := newPublishFixture(t)
		p.proveRoute()
		p.approvalFact("turn-1")
		bid := bindingIDOf(t, p.fixture, p.principalOf(p.reader))
		var token string
		for _, ch := range decode[OwnerWhoResult](t, p.owner(OwnerWho{}, "")).Channels {
			for _, m := range ch.Members {
				if m.Alias == "reader" {
					token = m.DepartToken
				}
			}
		}
		if owner, _ := p.ownerAndFact(); !owner {
			t.Fatal("fixture: the owner does not see the approval")
		}
		wantOK(t, p.owner(OwnerDepart{BindingID: bid, Seen: token}, "depart"))
		if owner, _ := p.ownerAndFact(); owner {
			t.Fatal("a departed member still waits on the owner")
		}
	})
}

// Codex runs PermissionRequest also for escalations it approves on its own,
// so its approval fact never says that the turn waits on the owner, and it
// keeps the fact's cap.
func TestACodexApprovalRequestDoesNotSayTheTurnWaitsOnTheOwner(t *testing.T) {
	f, writer := adapterChannel(t)
	f.policy("codex", true, 1)
	s := f.newSession("codex")
	at := f.now.UnixMilli()
	f.hook(s, codexStart("startup", publish.ModeDaemon, at))
	alias := decode[JoinResult](t, f.run(s, Join{Channel: "checkout"}, "")).Alias
	// No crosshall command of the member follows the request.
	approval := event(SessionLaunch, FactApprovalRequested, "t1:approval", "t1", at+1, false)
	approval.Profile = codexProfile(publish.ModeDaemon)
	f.hook(s, approval)
	fact, err := latestFact(t.Context(), f.db, f.principalOf(s))
	if err != nil || fact == nil || fact.Fact != FactApprovalRequested {
		t.Fatalf("fixture fact %+v, %v", fact, err)
	}
	send := decode[AskResult](t, f.ask(writer, Ask{To: []string{alias}, New: true, Body: "work"}, "codex")).Recipients[0]
	if fact.ApprovalOpen || send.Wake.Why == WhyApproval {
		t.Fatalf("a Codex approval request: fact %+v, send %+v", fact, send.Wake)
	}
}
