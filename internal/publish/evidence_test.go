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
