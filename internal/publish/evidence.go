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

// ShippedEvidence lists the evidence the shipped adapters register.
func ShippedEvidence() []Evidence { return Shipped().Evidence() }
