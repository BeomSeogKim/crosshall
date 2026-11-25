package domain

import (
	"context"

	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

type BuiltinEvidence = store.BuiltinEvidence

// BuiltinCapabilityEvidence is the evidence every adapter ships beside
// itself (publish.ShippedEvidence), as the store seeds it.
var BuiltinCapabilityEvidence = builtinEvidence(publish.ShippedEvidence())

func builtinEvidence(rows []publish.Evidence) []BuiltinEvidence {
	out := make([]BuiltinEvidence, 0, len(rows))
	for _, r := range rows {
		out = append(out, BuiltinEvidence{Vendor: r.Vendor, Version: r.Version, Platform: r.Platform, Mode: r.Mode,
			ManifestRev: r.ManifestRev, Capability: r.Capability, ResultRef: r.ResultRef, CheckedAtMS: r.CheckedAtMS})
	}
	return out
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
