package publish

import (
	"slices"
	"strings"
	"testing"
)

// Each adapter registers its own rows, which name it, through its publish
// adapter or, without one, beside it; a build and
// capability has at most one shipped row.
func TestShippedEvidenceIsRegisteredByItsAdapter(t *testing.T) {
	type key struct{ vendor, version, platform, mode, rev, capability string }
	seen := map[key]bool{}
	rows := map[string][]Evidence{}
	for name, a := range Shipped() {
		rows[name] = a.Evidence()
	}
	for name, evidence := range evidenceOnly {
		if _, ok := rows[name]; ok {
			t.Errorf("%s registers evidence twice", name)
		}
		rows[name] = evidence
	}
	for name, evidence := range rows {
		for _, r := range evidence {
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

// Every Reach reason a shipped adapter gives is one it declares, word for
// word, and none is a core reason. The core classifies a reason where the
// adapter's Reach gives it, never by these words.
func TestReachReasonsAreDeclaredWordForWord(t *testing.T) {
	codex, _ := Shipped().Lookup("codex")
	if got := codex.ReachReasons(); !slices.Equal(got, []string{"codex runs without its daemon", "codex daemon not confirmed"}) {
		t.Errorf("codex declares %q", got)
	}
	for _, mode := range []string{ModeInProcess, ModeDaemon, "unverified", ""} {
		for name, a := range Shipped() {
			if why := a.Reach(Route{ThreadID: "t", Mode: mode}); why != "" && !slices.Contains(a.ReachReasons(), why) {
				t.Errorf("%s reach reason %q not declared for it", name, why)
			}
		}
	}
	for name, a := range Shipped() {
		if slices.Contains(a.ReachReasons(), "route not proven") || slices.Contains(a.ReachReasons(), "") {
			t.Errorf("%s declares a core reason: %q", name, a.ReachReasons())
		}
	}
}

// A new adapter needs no central edit: registering it is enough for its
// reach reasons to be read as its own and its evidence to be seeded.
func TestANewAdapterRegistersItsReachReasonsAndEvidence(t *testing.T) {
	r := Shipped().With("fixture", fixture{})
	if a, ok := r.Lookup("fixture"); !ok || !slices.Contains(a.ReachReasons(), "fixture cannot reach it") {
		t.Fatal("fixture not registered with its reach reason")
	}
	if !slices.ContainsFunc(r.Evidence(), func(e Evidence) bool { return e.Vendor == "fixture" }) {
		t.Fatal("fixture evidence not listed")
	}
	if _, ok := Lookup("fixture"); ok {
		t.Fatal("an unregistered adapter was found before Swap")
	}
	restore := Swap(r)
	defer restore()
	if _, ok := Lookup("fixture"); !ok {
		t.Fatal("fixture not found after Swap")
	}
}
