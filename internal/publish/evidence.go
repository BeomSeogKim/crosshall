package publish

import (
	"maps"
	"slices"
	"strings"
)

// Evidence is one shipped test result for a vendor build: which build
// (version, platform, mode and hook manifest revision), which capability
// it proves, the run that proved it and when. Each adapter keeps its own
// rows beside it; the dispatcher seeds them, and an owner's own evidence
// takes precedence.
type Evidence struct {
	Vendor, Version, Platform, Mode, ManifestRev string
	Capability, ResultRef                        string
	CheckedAtMS                                  int64
}

// evidenceOnly holds the rows of adapters that have shipped evidence but no
// publish adapter: registering one in Shipped would make it a publish
// adapter everywhere the registry is read.
var evidenceOnly = map[string][]Evidence{"agy": agyEvidence}

// ShippedEvidence lists the evidence the shipped adapters register, in
// adapter name order.
func ShippedEvidence() []Evidence {
	out := Shipped().Evidence()
	for _, name := range slices.Sorted(maps.Keys(evidenceOnly)) {
		out = append(out, evidenceOnly[name]...)
	}
	slices.SortStableFunc(out, func(a, b Evidence) int { return strings.Compare(a.Vendor, b.Vendor) })
	return out
}
