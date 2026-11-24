package domain

import (
	"slices"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
	"github.com/BeomSeogKim/crosshall/internal/publish"
)

// Every built-in evidence row names a revision the catalog knows and one of
// the store's capability names, and a publish row names an adapter that has
// a publish adapter.
func TestBuiltinEvidenceLinksToTheCatalog(t *testing.T) {
	caps := []string{"enroll", "observe", "boundary", "reminder", "turn_end", "publish"}
	for _, row := range BuiltinCapabilityEvidence {
		if _, ok := manifests.CapabilitiesAt(row.Vendor, row.ManifestRev); !ok {
			t.Errorf("%s: %s/%s is not a revision the catalog knows", row.ResultRef, row.Vendor, row.ManifestRev)
		}
		if !slices.Contains(caps, row.Capability) {
			t.Errorf("%s: unknown capability %q", row.ResultRef, row.Capability)
		}
		if _, sends := publish.Lookup(row.Vendor); row.Capability == "publish" && !sends {
			t.Errorf("%s: publish evidence for %s, which has no publish adapter", row.ResultRef, row.Vendor)
		}
	}
}
