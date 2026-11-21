package domain

import (
	"context"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

func recordPublishProbation(ctx context.Context, tx store.Tx, attempt int64, observation string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO capability_build
		(vendor, vendor_version, platform, mode, manifest_rev, cap, confirmed, stopped_at_ms, stop_reason, stop_ref)
		SELECT r.adapter, json_extract(l.profile, '$.vendor_version'), json_extract(l.profile, '$.platform'),
			json_extract(l.profile, '$.mode'), json_extract(l.profile, '$.manifest_rev'), 'publish', ?2 = 'own_turn',
			CASE WHEN ?2 <> 'own_turn' THEN ?3 END, NULLIF(?2, 'own_turn'), CASE WHEN ?2 <> 'own_turn' THEN r.id END
		FROM publish_attempt r JOIN adapter_launch l ON l.principal_id = r.principal_id AND l.launch_ref = r.launch_ref
		WHERE r.id = ?1 AND l.verified = 1 AND `+store.CapabilityCovered("r.adapter", "l.profile", "'publish'")+`
		AND NOT EXISTS (SELECT 1 FROM capability_evidence e
			WHERE (e.vendor, e.vendor_version, e.platform, e.mode, e.manifest_rev, e.cap) =
			(r.adapter, json_extract(l.profile, '$.vendor_version'), json_extract(l.profile, '$.platform'),
			json_extract(l.profile, '$.mode'), json_extract(l.profile, '$.manifest_rev'), 'publish'))
		ON CONFLICT (vendor, vendor_version, platform, mode, manifest_rev, cap) DO UPDATE SET
			confirmed = capability_build.confirmed + excluded.confirmed,
			stopped_at_ms = COALESCE(excluded.stopped_at_ms, capability_build.stopped_at_ms),
			stop_reason = COALESCE(excluded.stop_reason, capability_build.stop_reason),
			stop_ref = COALESCE(excluded.stop_ref, capability_build.stop_ref),
			lifted_at_ms = CASE WHEN excluded.stop_ref IS NULL THEN capability_build.lifted_at_ms END
		WHERE capability_build.confirmed < ?4`, attempt, observation, ms(now), store.PublishBuildConfirmations)
	return err
}

func liftPublishBuildForBinding(ctx context.Context, tx store.Tx, binding int64, now time.Time) (bool, error) {
	res, err := tx.ExecContext(ctx, `UPDATE capability_build AS cb SET lifted_at_ms = MAX(?1, stopped_at_ms)
		WHERE cb.cap = 'publish' AND `+store.CapabilityBuildStopped("cb")+` AND EXISTS (
			SELECT 1 FROM binding b JOIN adapter_launch l ON l.rowid = (
				SELECT latest.rowid FROM adapter_launch latest
				WHERE latest.principal_id = b.principal_id AND latest.ended_hook_at_ms IS NULL
				ORDER BY latest.started_hook_at_ms DESC, latest.rowid DESC LIMIT 1)
			WHERE b.id = ?2 AND b.state = 'live' AND l.verified = 1 AND l.fenced = 0
			  AND cb.vendor = json_extract(l.profile, '$.adapter')
			  AND cb.vendor_version = json_extract(l.profile, '$.vendor_version')
			  AND cb.platform = json_extract(l.profile, '$.platform') AND cb.mode = json_extract(l.profile, '$.mode')
			  AND cb.manifest_rev = json_extract(l.profile, '$.manifest_rev'))`, ms(now), binding)
	if err != nil {
		return false, err
	}
	return changedOne(res)
}

func liftPublishBuildForEvidence(ctx context.Context, tx store.Tx, evidence AdapterEvidence, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE capability_build AS cb SET lifted_at_ms = MAX(?, stopped_at_ms)
		WHERE vendor = ? AND vendor_version = ? AND platform = ? AND mode = ? AND manifest_rev = ?
		AND cap = 'publish' AND stopped_at_ms < ? AND `+store.CapabilityBuildStopped("cb"),
		ms(now), evidence.Vendor, evidence.VendorVersion, evidence.Platform, evidence.Mode, evidence.ManifestRev, evidence.RunAtMS)
	return err
}
