package domain

import (
	"context"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

// PublishProfile is a vendor profile that an interactive conformance run
// proved for publish (design section 8), with that run's reference and time.
type PublishProfile struct {
	Vendor, Version, Platform, Mode, ManifestRev string
	ResultRef                                    string
	CheckedAtMS                                  int64
}

// TestedPublishProfiles ships with the binary, so a stock install needs no
// import; an owner's evidence import only adds to it. It is empty until a
// sender passes its interactive conformance run.
var TestedPublishProfiles []PublishProfile

// SeedPublishProfiles records the shipped profiles as tested publish
// evidence, marked as shipped and dated by their conformance run, so a
// restart never makes them look newer. Shipped rows this binary no longer
// ships are removed; an owner's row for the same profile is kept as it is.
func SeedPublishProfiles(ctx context.Context, tx store.Tx, profiles []PublishProfile) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM capability_evidence WHERE cap = 'publish' AND source = 'builtin'`); err != nil {
		return err
	}
	for _, p := range profiles {
		if _, err := tx.ExecContext(ctx, `INSERT INTO capability_evidence
			(vendor, vendor_version, platform, mode, manifest_rev, cap, level, result_ref, checked_at_ms, source)
			VALUES (?, ?, ?, ?, ?, 'publish', 'tested', ?, ?, 'builtin') ON CONFLICT DO NOTHING`,
			p.Vendor, p.Version, p.Platform, p.Mode, p.ManifestRev, p.ResultRef, p.CheckedAtMS); err != nil {
			return err
		}
	}
	return nil
}
