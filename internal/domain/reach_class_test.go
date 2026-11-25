package domain

import (
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// newVendor is an adapter registered under a name the core never saw,
// with a reach reason of its own.
type newVendor struct{ publish.Adapter }

func (newVendor) ReachReasons() []string { return []string{"newvendor has no socket"} }

// A sender the core never saw names its own unreachable sessions: its
// reach reason reads no_route with its adapter ID, with no edit to the
// core, while a shipped reason keeps its class.
func TestAReachReasonIsReadForTheAdapterThatRegistersIt(t *testing.T) {
	codex, _ := publish.Shipped().Lookup("codex")
	restore := publish.Swap(publish.Shipped().With("newvendor", newVendor{codex}))
	defer restore()
	for reason, adapter := range map[string]string{"newvendor has no socket": "newvendor", publish.CodexNoDaemon: "codex"} {
		w := reachWake(store.PublishState{State: "candidate", Reason: reason}, true)
		if w.Why != WhyNoRoute || w.Adapter != adapter {
			t.Errorf("reason %q: %+v, want no_route for %s", reason, w, adapter)
		}
	}
	if w := reachWake(store.PublishState{State: "candidate", Reason: "route not proven"}, true); w.Why != WhyUnproven || w.Adapter != "" {
		t.Errorf("core reason: %+v", w)
	}
}
