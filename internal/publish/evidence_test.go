package publish

import (
	"slices"
	"strings"
	"testing"
)

// Each adapter registers its own rows, which name it; a build and
// capability has at most one shipped row.
func TestShippedEvidenceIsRegisteredByItsAdapter(t *testing.T) {
	type key struct{ vendor, version, platform, mode, rev, capability string }
	seen := map[key]bool{}
	for name, a := range Shipped() {
		for _, r := range a.Evidence() {
			if r.Vendor != name {
				t.Errorf("%s row names %q", name, r.Vendor)
			}
			k := key{r.Vendor, r.Version, r.Platform, r.Mode, r.ManifestRev, r.Capability}
			if seen[k] {
				t.Errorf("two rows for %+v", k)
			}
			seen[k] = true
			if r.ResultRef == "" || r.CheckedAtMS <= 0 {
				t.Errorf("row without its run: %+v", r)
			}
		}
	}
	all := ShippedEvidence()
	if len(all) != len(seen) || !slices.IsSortedFunc(all, func(a, b Evidence) int { return strings.Compare(a.Vendor, b.Vendor) }) {
		t.Fatalf("shipped evidence %d rows, adapters out of order", len(all))
	}
}

// Every shipped Reach reason is declared by its adapter word for word, so a
// stored reason keeps its class across releases.
func TestReachReasonsAreDeclaredWordForWord(t *testing.T) {
	for reason, want := range map[string]string{
		"codex runs without its daemon": "codex",
		"codex daemon not confirmed":    "codex",
	} {
		if got, ok := ReachReason(reason); !ok || got != want {
			t.Errorf("ReachReason(%q) = %q, %v", reason, got, ok)
		}
	}
	for _, mode := range []string{ModeInProcess, ModeDaemon, "unverified", ""} {
		for name, a := range Shipped() {
			if why := a.Reach(Route{ThreadID: "t", Mode: mode}); why != "" {
				if owner, ok := ReachReason(why); !ok || owner != name {
					t.Errorf("%s reach reason %q not declared for it", name, why)
				}
			}
		}
	}
	for _, reason := range []string{"route not proven", ""} {
		if _, ok := ReachReason(reason); ok {
			t.Errorf("%q read as a reach reason", reason)
		}
	}
}

// A new adapter needs no central edit: registering it is enough for its
// reach reasons to be read as its own and its evidence to be seeded.
func TestANewAdapterRegistersItsReachReasonsAndEvidence(t *testing.T) {
	r := Shipped().With("fixture", fixture{})
	if got, ok := r.ReachReason("fixture cannot reach it"); !ok || got != "fixture" {
		t.Fatalf("fixture reach reason = %q, %v", got, ok)
	}
	if !slices.ContainsFunc(r.Evidence(), func(e Evidence) bool { return e.Vendor == "fixture" }) {
		t.Fatal("fixture evidence not listed")
	}
	if _, ok := ReachReason("fixture cannot reach it"); ok {
		t.Fatal("an unregistered adapter's reason was read before Swap")
	}
	restore := Swap(r)
	defer restore()
	if got, ok := ReachReason("fixture cannot reach it"); !ok || got != "fixture" {
		t.Fatalf("after Swap: %q, %v", got, ok)
	}
}
