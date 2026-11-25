package publish

// claudeEvidence is Claude Code's shipped evidence.
var claudeEvidence = []Evidence{
	{Vendor: "claude", Version: "2.1.330", Platform: "darwin-arm64", Mode: "print", ManifestRev: "claude-2",
		Capability: "turn_end", ResultRef: "conformance:TestClaudeCodeTurnEndConformance@a2d52cb", CheckedAtMS: 1795241413595},
	{Vendor: "claude", Version: "2.1.330", Platform: "darwin-arm64", Mode: "interactive", ManifestRev: "claude-2",
		Capability: "turn_end", ResultRef: "conformance:TestClaudeCodeInteractiveTurnEndConformance@a2d52cb", CheckedAtMS: 1795241385990},
	{Vendor: "claude", Version: "2.1.330", Platform: "darwin-arm64", Mode: "interactive", ManifestRev: "claude-2",
		Capability: "publish", ResultRef: "trial:publish-matrix@c1db3a4", CheckedAtMS: 1795242199565},
	{Vendor: "claude", Version: "2.1.331", Platform: "darwin-arm64", Mode: "print", ManifestRev: "claude-3",
		Capability: "turn_end", ResultRef: "conformance:TestClaudeCodeTurnEndConformance@87797cf", CheckedAtMS: 1795476417385},
	{Vendor: "claude", Version: "2.1.331", Platform: "darwin-arm64", Mode: "interactive", ManifestRev: "claude-3",
		Capability: "turn_end", ResultRef: "conformance:TestClaudeCodeInteractiveTurnEndConformance@87797cf", CheckedAtMS: 1795477207405},
	{Vendor: "claude", Version: "2.1.331", Platform: "darwin-arm64", Mode: "interactive", ManifestRev: "claude-3",
		Capability: "publish", ResultRef: "trial:publish-matrix@0132580", CheckedAtMS: 1795520993007},
}
