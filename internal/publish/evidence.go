package publish

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

// shippedEvidence holds each adapter's rows, by adapter name.
var shippedEvidence = map[string][]Evidence{
	"claude": claudeEvidence,
	"codex":  codexEvidence,
}

// ShippedEvidence lists every adapter's shipped rows, adapters in name
// order and each adapter's rows in its own order.
func ShippedEvidence() []Evidence {
	var out []Evidence
	for _, name := range Shipped().Names() {
		out = append(out, shippedEvidence[name]...)
	}
	return out
}
