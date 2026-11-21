package store

import (
	"context"
	"fmt"
)

const PublishBuildConfirmations = 3

// CapabilityCovered matches tested evidence by manifest revision unless the
// exact build has evidence of its own. The arguments are SQL expressions
// supplied by callers, which bind runtime values separately.
func CapabilityCovered(vendor, profile, capability string) string {
	version := profileField(profile, "vendor_version")
	mode := profileField(profile, "mode")
	return fmt.Sprintf(`COALESCE((%s NOT IN ('', 'unverified') AND %s NOT IN ('', 'unverified')
		AND NOT EXISTS (SELECT 1 FROM capability_build cb WHERE %s AND cb.vendor_version = %s AND %s)
		AND COALESCE(
			(SELECT exact.level = 'tested' FROM capability_evidence exact WHERE %s AND exact.vendor_version = %s),
			EXISTS (SELECT 1 FROM capability_evidence tested WHERE %s AND tested.level = 'tested')
		)), 0)`, version, mode, capabilityProfileMatch("cb", vendor, profile, capability), version, CapabilityBuildStopped("cb"),
		capabilityProfileMatch("exact", vendor, profile, capability), version,
		capabilityProfileMatch("tested", vendor, profile, capability))
}

func profileField(profile, field string) string {
	return fmt.Sprintf("json_extract(%s, '$.%s')", profile, field)
}

func capabilityProfileMatch(alias, vendor, profile, capability string) string {
	return fmt.Sprintf(`%[1]s.vendor = %[2]s AND %[1]s.cap = %[3]s AND %[1]s.platform = %[4]s
		AND %[1]s.mode = %[5]s AND %[1]s.manifest_rev = %[6]s`, alias, vendor, capability,
		profileField(profile, "platform"), profileField(profile, "mode"), profileField(profile, "manifest_rev"))
}

// CapabilityBuildStopped treats a lift in the stop's millisecond as effective.
// Writes in that millisecond are ordered by the store transaction.
func CapabilityBuildStopped(alias string) string {
	return fmt.Sprintf(`%[1]s.stopped_at_ms IS NOT NULL
		AND (%[1]s.lifted_at_ms IS NULL OR %[1]s.lifted_at_ms < %[1]s.stopped_at_ms)`, alias)
}

type capabilityBuildEvidence struct {
	Version, Platform, Mode, ManifestRev string
	ExactLevel, TestedFrom               string
	Covered, Stopped                     bool
	Confirmed                            int
	StoppedAtMS, Notice                  int64
	StopReason                           string
}

func readCapabilityBuild(ctx context.Context, tx Tx, vendor, profile, capability string) (capabilityBuildEvidence, error) {
	var build capabilityBuildEvidence
	err := tx.QueryRowContext(ctx, `SELECT
		COALESCE(json_extract(?2, '$.vendor_version'), ''), COALESCE(json_extract(?2, '$.platform'), ''),
		COALESCE(json_extract(?2, '$.mode'), ''), COALESCE(json_extract(?2, '$.manifest_rev'), ''),
		`+CapabilityCovered("?1", "?2", "?3")+`,
		COALESCE((SELECT e.level FROM capability_evidence e WHERE `+capabilityProfileMatch("e", "?1", "?2", "?3")+`
			AND e.vendor_version = json_extract(?2, '$.vendor_version')), ''),
		COALESCE((SELECT e.vendor_version FROM capability_evidence e WHERE `+capabilityProfileMatch("e", "?1", "?2", "?3")+`
			AND e.level = 'tested' ORDER BY e.checked_at_ms DESC, e.vendor_version LIMIT 1), ''),
		COALESCE(cb.confirmed, 0), `+CapabilityBuildStopped("cb")+`, COALESCE(cb.stopped_at_ms, 0),
		COALESCE(cb.stop_ref, 0), COALESCE(cb.stop_reason, '')
		FROM (SELECT 1) LEFT JOIN capability_build cb ON `+capabilityProfileMatch("cb", "?1", "?2", "?3")+`
			AND cb.vendor_version = json_extract(?2, '$.vendor_version')`, vendor, profile, capability).
		Scan(&build.Version, &build.Platform, &build.Mode, &build.ManifestRev, &build.Covered, &build.ExactLevel,
			&build.TestedFrom, &build.Confirmed, &build.Stopped, &build.StoppedAtMS, &build.Notice, &build.StopReason)
	return build, err
}
