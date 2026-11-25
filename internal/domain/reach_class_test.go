package domain

import (
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/publish"
)

// reachVendor is an adapter registered under a name the core never saw,
// whose sender reaches no session, with the reason it gives.
type reachVendor struct {
	publish.Adapter
	reason string
}

func (a reachVendor) Reach(publish.Route) string { return a.reason }
func (a reachVendor) ReachReasons() []string     { return []string{a.reason} }
func (reachVendor) Evidence() []publish.Evidence { return nil }

// statusWake is the reader's wake on the newest request in the status view.
func (p *publishFixture) statusWake() Wake {
	p.t.Helper()
	var wake Wake
	found := false
	for _, channel := range decode[StatusResult](p.t, p.run(p.writer, Status{}, "")).Channels {
		for _, request := range channel.Requests {
			for _, r := range request.Recipients {
				if r.Alias == "reader" {
					wake, found = r.Wake, true
				}
			}
		}
	}
	if !found {
		p.t.Fatal("no request to @reader in status")
	}
	return wake
}

// A session is unreachable for the adapter whose own sender said so, read
// where its Reach reports it and not from the reason's words: an adapter the
// core never saw names its sessions with no edit to the core, two adapters
// may use the same words, and a core reason keeps its class whatever an
// adapter declares (#359 review). Send, who, waiting and status agree.
func TestAnUnreachableSessionIsNamedForTheAdapterThatReportsIt(t *testing.T) {
	for _, tc := range []struct {
		name, reason, routeAdapter string
		want                       Wake
	}{
		{"its own words", "newvendor has no socket", "newvendor", Wake{Wake: WakeNone, Why: WhyNoRoute, Adapter: "newvendor", SeenAt: SeenAtPrompt}},
		{"a shipped adapter's words", publish.CodexNoDaemon, "newvendor", Wake{Wake: WakeNone, Why: WhyNoRoute, Adapter: "newvendor", SeenAt: SeenAtPrompt}},
		{"a core reason's words", "route not proven", "claude", notWoken(WhyUnproven)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := wakeFixture(t)
			claude, _ := publish.Shipped().Lookup("claude")
			t.Cleanup(publish.Swap(publish.Adapters().With("newvendor", reachVendor{claude, tc.reason})))
			if tc.routeAdapter != "claude" {
				p.policy(tc.routeAdapter, true, 1)
				p.mutate("UPDATE publish_route SET adapter = ? WHERE binding_id = ?", tc.routeAdapter, p.binding.ID)
			}
			if status := p.publishState(); status.State != "candidate" || status.Reason != tc.reason {
				t.Fatalf("publish state %+v, want candidate %q", status, tc.reason)
			}
			p.sendTo(tc.name, tc.want)
			if got := p.statusWake(); got != tc.want {
				t.Fatalf("status shows %+v, want %+v", got, tc.want)
			}
		})
	}
}
