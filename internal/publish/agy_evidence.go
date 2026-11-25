package publish

// agyEvidence is Antigravity CLI's shipped evidence. Antigravity has no
// sender, so it registers its rows without a publish adapter.
var agyEvidence = []Evidence{
	{Vendor: "agy", Version: "1.5.16", Platform: "darwin-arm64", Mode: "interactive", ManifestRev: "agy-2",
		Capability: "turn_end", ResultRef: "conformance:TestAntigravityInteractiveTurnEndConformance@d90021d", CheckedAtMS: 1795588229635},
}
