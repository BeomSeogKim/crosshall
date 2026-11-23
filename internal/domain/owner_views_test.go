package domain

import (
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
