package domain

import (
	"fmt"
	"testing"
	"time"
)

func TestOwnerWhoReportsHowEachBindingGetsWork(t *testing.T) {
	p := newPublishFixture(t)
	p.svc.HasPromptLine = func(adapter, revision string) bool {
		return adapter == testProfile.Adapter && revision == testProfile.ManifestRev
	}
	p.proveRoute()
	check := func(alias, reach, route string) {
		t.Helper()
		result := p.owner(OwnerWho{}, "")
		wantOK(t, result)
		for _, channel := range decode[OwnerWhoResult](t, result).Channels {
			for _, member := range channel.Members {
				if member.Alias == alias {
					if member.Reach != reach || member.Route != route {
						t.Fatalf("@%s reach=%q route=%q, want %q and %q", alias, member.Reach, member.Route, reach, route)
					}
					return
				}
			}
		}
		t.Fatalf("missing @%s", alias)
	}
	check("writer", "pull", "pull")
	check("reader", "notice", "pull")
	wantOK(t, p.owner(OwnerPublish{Hall: true, Switch: "off"}, "reach-hall-off"))
	check("reader", "prompt", "pull")
	p.mutate(`UPDATE binding SET dnd = 1 WHERE id = ?`, p.binding.ID)
	check("reader", "pull", "pull")
	p.mutate(`UPDATE binding SET dnd = 0 WHERE id = ?`, p.binding.ID)
	p.mutate(`UPDATE adapter_policy SET enabled = 0 WHERE adapter = 'claude'`)
	check("reader", "pull", "pull")
	p.mutate(`UPDATE adapter_policy SET enabled = 1 WHERE adapter = 'claude'`)
	p.mutate(`UPDATE adapter_launch SET fenced = 1 WHERE launch_ref = ?`, publishTestLaunch)
	check("reader", "pull", "pull")
	wantOK(t, p.run(p.reader, Leave{}, "reach-leave"))
	check("reader", "", "unavailable")
}

func TestOwnerWhoProjectsEvidenceAndOpenRequests(t *testing.T) {
	f, writer := adapterChannel(t)
	member := f.newSession("claude")
	joinedAt := f.now
	f.hook(member, start("pid-1", "startup", ms(joinedAt)))
	f.hook(member, prompt("pid-1", "t1", ms(joinedAt)+1000))
	memberRow := func() OwnerMember {
		t.Helper()
		r := f.owner(OwnerWho{}, "")
		wantOK(t, r)
		for _, ch := range decode[OwnerWhoResult](t, r).Channels {
			for _, m := range ch.Members {
				if ch.Name == "checkout" && m.Alias == "claude1" {
					return m
				}
			}
		}
		t.Fatal("@claude1 missing from #checkout")
		return OwnerMember{}
	}
	m := memberRow()
	if m.LastEvidence != nil || m.EvidenceStale || m.OpenRequests == nil || len(m.OpenRequests) != 0 {
		t.Fatalf("new binding: %+v", m)
	}
	if m.Observed == nil || m.Observed.Fact != FactPromptSubmitted || m.Observed.Verified {
		t.Fatalf("unverified adapter observation: %+v", m.Observed)
	}
	if !ValidDepartureToken(m.DepartToken) {
		t.Fatalf("departure token %q", m.DepartToken)
	}
	f.now = joinedAt.Add(30 * time.Minute)
	if m = memberRow(); m.LastEvidence != nil || !m.EvidenceStale || m.BindingState != "live" {
		t.Fatalf("binding without recent evidence: %+v", m)
	}

	request := f.ask(writer, Ask{To: []string{"claude1"}, Body: "review?"}, "")
	wantOK(t, request)
	id := decode[AskResult](t, request).ID
	wantOK(t, f.who(member))
	commandAt := f.now
	f.hook(member, prompt("pid-1", "t2", ms(commandAt)+1000))
	m = memberRow()
	wantEvidence := OwnerEvidence{Fact: "command", Source: "cli", AtMS: ms(commandAt), AgeS: 1}
	if m.LastEvidence == nil || *m.LastEvidence != wantEvidence || m.EvidenceStale {
		t.Fatalf("recent command: %+v, stale %t", m.LastEvidence, m.EvidenceStale)
	}
	if len(m.OpenRequests) != 1 || m.OpenRequests[0] != id {
		t.Fatalf("open requests: %v, want [%d]", m.OpenRequests, id)
	}
	f.now = commandAt.Add(30 * time.Minute)
	m = memberRow()
	wantEvidence.AgeS = 1800
	if m.LastEvidence == nil || *m.LastEvidence != wantEvidence || !m.EvidenceStale || m.BindingState != "live" {
		t.Fatalf("expired command: %+v, stale %t, state %q", m.LastEvidence, m.EvidenceStale, m.BindingState)
	}
	f.pickUp(member, id)
	wantOK(t, f.reply(member, Reply{To: id, Body: "done"}, ""))
	if m = memberRow(); m.OpenRequests == nil || len(m.OpenRequests) != 0 {
		t.Fatalf("open requests after settlement: %v", m.OpenRequests)
	}
}

func (f *fixture) ownerRow(alias string) OwnerMember {
	f.t.Helper()
	for _, ch := range decode[OwnerWhoResult](f.t, f.owner(OwnerWho{}, "")).Channels {
		for _, m := range ch.Members {
			if m.Alias == alias {
				return m
			}
		}
	}
	f.t.Fatalf("owner who lists no @%s", alias)
	return OwnerMember{}
}

// owner who counts a member's unread requests that nothing will tell it
// about, so the owner finds stuck work without reading each line.
func TestOwnerWhoCountsUnreadRequestsNothingWillTell(t *testing.T) {
	p := newPublishFixture(t)
	first := p.now
	if got := p.ownerRow("reader").UnreadNotWoken; got == nil || got.Requests != 1 || got.OldestAtMS != first.UnixMilli() || got.OldestAgeS != 0 {
		t.Fatalf("one unread request before any route: %+v", got)
	}
	p.now = p.now.Add(10 * time.Minute)
	second := p.turnEndAsk(p.writer, "reader", "second")
	wantOK(t, p.run(p.writer, Note{To: []string{"reader"}, Body: "fyi"}, "note")) // a note is not a request
	if got := p.ownerRow("reader").UnreadNotWoken; got == nil || got.Requests != 2 || got.OldestAgeS != 600 {
		t.Fatalf("two unread requests, the oldest ten minutes old: %+v", got)
	}
	if got := p.ownerRow("writer").UnreadNotWoken; got != nil {
		t.Fatalf("a member without requests: %+v", got)
	}

	wantOK(t, p.run(p.reader, Read{IDs: []int64{p.beforeID}}, ""))
	if got := p.ownerRow("reader").UnreadNotWoken; got == nil || got.Requests != 1 || got.OldestAtMS != p.now.UnixMilli() {
		t.Fatalf("after reading the first, only #%d: %+v", second, got)
	}

	p.proveRoute()
	p.turnEndAsk(p.writer, "reader", "noticed")
	// The second, stored while the route awaited its proof, is offered too (#484).
	if got := p.ownerRow("reader").UnreadNotWoken; got != nil {
		t.Fatalf("a request a notice will offer is counted: %+v", got)
	}

	wantOK(t, p.run(p.reader, Leave{}, "leave"))
	if got := p.ownerRow("reader").UnreadNotWoken; got != nil {
		t.Fatalf("a departed member: %+v", got)
	}
}

// A request the member's turn end will offer is not stuck.
func TestOwnerWhoLeavesOutARequestTheTurnEndWillOffer(t *testing.T) {
	f, _, writer := turnEndFixture(t, "launch")
	wantOK(t, f.ask(writer, Ask{To: []string{"reader"}, New: true, Body: "work"}, "work"))
	if got := f.ownerRow("reader").UnreadNotWoken; got == nil || got.Requests != 1 {
		t.Fatalf("without turn-end evidence: %+v", got)
	}
	wantOK(t, f.owner(turnEndEvidence("tested"), "evidence"))
	if got := f.ownerRow("reader").UnreadNotWoken; got != nil {
		t.Fatalf("in a checked turn: %+v", got)
	}
}

// A plain wait returns only messages above its cursor, so it does not
// tell its member about a request that came before; one sent while it
// runs, it does.
func TestOwnerWhoCountsARequestBehindAWaitsCursor(t *testing.T) {
	f, writer, reader := waitPresenceChannel(t)
	wantOK(t, f.ask(writer, Ask{To: []string{"reader"}, Body: "older"}, "older"))
	poll(t, f, reader, Wait{})
	if got := f.ownerRow("reader").UnreadNotWoken; got == nil || got.Requests != 1 {
		t.Fatalf("a request behind the running wait's cursor: %+v", got)
	}
	wantOK(t, f.ask(writer, Ask{To: []string{"reader"}, Body: "newer"}, "newer"))
	if got := f.ownerRow("reader").UnreadNotWoken; got == nil || got.Requests != 1 {
		t.Fatalf("a request the running wait will return was counted: %+v", got)
	}
}

// A request the member already answered with more time is not stuck: its
// sender sees more_time, not a warning.
func TestOwnerWhoLeavesOutARequestGivenMoreTime(t *testing.T) {
	f, writer, reader := waitPresenceChannel(t)
	ask := decode[AskResult](t, f.ask(writer, Ask{To: []string{"reader"}, Body: "work"}, "ask"))
	wantOK(t, actLedger(f, reader, Reply{To: ask.ID, Body: "later", Outcome: OutcomeMoreTime}, "more-time"))
	if got := f.ownerRow("reader").UnreadNotWoken; got != nil {
		t.Fatalf("a request given more time: %+v", got)
	}
}

// A turn end offers at most OfferCap requests, so the rest are stuck.
func TestOwnerWhoCountsRequestsPastTheTurnEndCap(t *testing.T) {
	f, _, writer := turnEndFixture(t, "launch")
	wantOK(t, f.owner(turnEndEvidence("tested"), "evidence"))
	var last int64
	for i := range OfferCap + 1 {
		f.now = f.now.Add(time.Second)
		last = decode[AskResult](t, f.ask(writer, Ask{To: []string{"reader"}, New: true, Body: "work"}, fmt.Sprintf("ask-%d", i))).ID
	}
	got := f.ownerRow("reader").UnreadNotWoken
	if got == nil || got.Requests != 1 || got.OldestAtMS != f.now.UnixMilli() {
		t.Fatalf("the request past the cap (#%d): %+v", last, got)
	}
}
