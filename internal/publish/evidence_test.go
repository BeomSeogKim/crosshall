package publish

import (
	"slices"
	"strings"
	"testing"
)

// Each adapter's rows sit beside it and name it; a build and capability
// has at most one shipped row; every adapter with rows is registered.
func TestShippedEvidenceSitsBesideItsAdapter(t *testing.T) {
	type key struct{ vendor, version, platform, mode, rev, capability string }
	seen := map[key]bool{}
	for name, rows := range shippedEvidence {
		if _, ok := Shipped().Lookup(name); !ok {
			t.Errorf("evidence for unregistered adapter %q", name)
		}
		for _, r := range rows {
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

// Every shipped Reach reason is declared with its adapter, word for word,
// so a stored reason keeps its class across releases (stored rows and
// v0.2.0 binaries read the same strings).
func TestReachReasonsAreDeclaredWordForWord(t *testing.T) {
	for reason, want := range map[string]string{
		"codex runs without its daemon": "codex",
		"codex daemon not confirmed":    "codex",
	} {
		if got, ok := ReachReason(reason); !ok || got != want {
			t.Errorf("ReachReason(%q) = %q, %v", reason, got, ok)
		}
	}
	for _, mode := range []string{ModeInProcess, "unverified", ""} {
		for name := range Shipped() {
			a, _ := Shipped().Lookup(name)
			if why := a.Reach(Route{ThreadID: "t", Mode: mode}); why != "" {
				if owner, ok := ReachReason(why); !ok || owner != name {
					t.Errorf("%s reach reason %q not declared for it", name, why)
				}
			}
		}
	}
	if _, ok := ReachReason("route not proven"); ok {
		t.Error("a core reason read as a reach reason")
	}
}
