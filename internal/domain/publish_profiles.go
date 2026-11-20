package domain

import (
	"context"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

// PublishProfile is a vendor profile that an interactive conformance run
// proved for publish (design section 8).
type PublishProfile struct {
	Vendor, Version, Platform, Mode, ManifestRev string
}

// TestedPublishProfiles ships with the binary, so a stock install needs no
// import; an owner's evidence import only adds to it. It is empty until a
// sender passes its interactive conformance run.
var TestedPublishProfiles []PublishProfile

// SeedPublishProfiles records the shipped profiles as tested publish
// evidence. Rows an earlier binary shipped that this one does not are
// removed; rows an owner imported are kept, and win over a shipped row.
func SeedPublishProfiles(ctx context.Context, tx store.Tx, profiles []PublishProfile, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM capability_evidence WHERE cap = 'publish' AND result_ref = 'builtin'`); err != nil {
		return err
	}
	for _, p := range profiles {
		if _, err := tx.ExecContext(ctx, `INSERT INTO capability_evidence
			(vendor, vendor_version, platform, mode, manifest_rev, cap, level, result_ref, checked_at_ms)
			VALUES (?, ?, ?, ?, ?, 'publish', 'tested', 'builtin', ?) ON CONFLICT DO NOTHING`,
			p.Vendor, p.Version, p.Platform, p.Mode, p.ManifestRev, ms(now)); err != nil {
			return err
		}
	}
	return nil
}
