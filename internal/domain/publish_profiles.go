package domain

import (
	"context"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

type BuiltinEvidence = store.BuiltinEvidence

var BuiltinCapabilityEvidence = []BuiltinEvidence{
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
	{Vendor: "codex", Version: "0.172.0", Platform: "darwin-arm64", Mode: "daemon", ManifestRev: "codex-1",
		Capability: "publish", ResultRef: "trial:TestCodexDaemonPublishTrial@ae995899a8e82b58121a738495ce1b17f83d3877", CheckedAtMS: 1795574350876},
}

// SeedBuiltinEvidence keeps run dates and owner overrides across restarts.
func SeedBuiltinEvidence(ctx context.Context, tx store.Tx, rows []BuiltinEvidence) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM capability_evidence WHERE source = 'builtin'`); err != nil {
		return err
	}
	for _, row := range rows {
		if _, err := tx.ExecContext(ctx, `INSERT INTO capability_evidence
			(vendor, vendor_version, platform, mode, manifest_rev, cap, level, result_ref, checked_at_ms, source)
			VALUES (?, ?, ?, ?, ?, ?, 'tested', ?, ?, 'builtin') ON CONFLICT DO NOTHING`,
			row.Vendor, row.Version, row.Platform, row.Mode, row.ManifestRev, row.Capability, row.ResultRef, row.CheckedAtMS); err != nil {
			return err
		}
	}
	return nil
}
