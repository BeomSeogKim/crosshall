package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"

	"github.com/BeomSeogKim/crosshall/internal/wire"
)

type InstallConsent struct {
	AutoJoinOff      bool
	PublishCovered   bool
	PublishSwitch    string
	PublishStopped   bool
	PublishProbation bool
	TurnEndCovered   bool
}

// InspectInstallConsent previews the settings and evidence after the new
// binary starts. It reads existing owner choices without creating a store.
func InspectInstallConsent(ctx context.Context, files fs.FS, profile wire.HookProfile, shipped []BuiltinEvidence) (InstallConsent, error) {
	profile.Mode = "interactive"
	launch, err := json.Marshal(profile)
	if err != nil {
		return InstallConsent{}, err
	}
	builtin, err := json.Marshal(shipped)
	if err != nil {
		return InstallConsent{}, err
	}
	var consent InstallConsent
	read := func(tx Tx) error {
		return readInstallConsent(ctx, tx, profile.Adapter, string(launch), string(builtin), &consent)
	}
	if files != nil {
		_, err := fs.Stat(files, "store.sqlite3")
		if !errors.Is(err, fs.ErrNotExist) {
			if err != nil {
				return consent, err
			}
			err = inspectStore(ctx, files, read)
			return consent, err
		}
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return consent, err
	}
	defer db.Close()
	err = read(db)
	return consent, err
}

func readInstallConsent(ctx context.Context, tx Tx, vendor, profile, builtin string, consent *InstallConsent) error {
	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version < 0 || version > len(migrations) {
		return fmt.Errorf("unsupported setup preview schema version %d", version)
	}
	owner := `SELECT '', '', '', '', '', '', '' WHERE 0`
	build := `SELECT '' vendor, '' vendor_version, '' platform, '' mode, '' manifest_rev, '' cap,
		0 confirmed, NULL stopped_at_ms, NULL lifted_at_ms WHERE 0`
	joining, publish := "0", "''"
	if version >= 2 {
		owner = `SELECT vendor, vendor_version, platform, mode, manifest_rev, cap, level FROM main.capability_evidence`
	}
	if version >= 10 {
		owner += ` WHERE source = 'owner'`
		publish = `CASE (SELECT publish FROM adapter_policy WHERE adapter = ?1)
			WHEN 0 THEN 'adapter_off'
			WHEN 1 THEN 'adapter_on'
			ELSE CASE (SELECT enabled FROM hall_publish WHERE id = 1) WHEN 0 THEN 'hall_off' ELSE '' END END`
	}
	if version >= 12 {
		fallback := "0"
		if version == 12 {
			fallback = `EXISTS (SELECT 1 FROM op WHERE verb = 'owner channel auto')`
		}
		joining = `COALESCE((SELECT enabled = 0 FROM hall_auto_join WHERE id = 1), ` + fallback + `)`
	}
	if version >= 16 {
		build = `SELECT * FROM main.capability_build`
	}
	printProfile := `json_set(?2, '$.mode', 'print')`
	return tx.QueryRowContext(ctx, `WITH
		owner_evidence(vendor, vendor_version, platform, mode, manifest_rev, cap, level) AS (`+owner+`),
		shipped(vendor, vendor_version, platform, mode, manifest_rev, cap) AS (
			SELECT value ->> '$.Vendor', value ->> '$.Version', value ->> '$.Platform',
				value ->> '$.Mode', value ->> '$.ManifestRev', value ->> '$.Capability'
			FROM json_each(?3)
		),
		capability_evidence AS (
			SELECT * FROM owner_evidence
			UNION ALL
			SELECT s.*, 'tested' FROM shipped s WHERE NOT EXISTS (
				SELECT 1 FROM owner_evidence e WHERE (e.vendor, e.vendor_version, e.platform, e.mode, e.manifest_rev, e.cap)
					= (s.vendor, s.vendor_version, s.platform, s.mode, s.manifest_rev, s.cap))
		),
		capability_build AS (`+build+`)
		SELECT (`+joining+`), `+CapabilityCovered("?1", "?2", "'publish'")+`, `+publish+`,
			EXISTS (SELECT 1 FROM capability_build cb WHERE `+capabilityProfileMatch("cb", "?1", "?2", "'publish'")+`
				AND cb.vendor_version = `+profileField("?2", "vendor_version")+` AND `+CapabilityBuildStopped("cb")+`),
			NOT EXISTS (SELECT 1 FROM capability_evidence e WHERE `+capabilityProfileMatch("e", "?1", "?2", "'publish'")+`
				AND e.vendor_version = `+profileField("?2", "vendor_version")+`)
			AND COALESCE((SELECT cb.confirmed FROM capability_build cb WHERE `+capabilityProfileMatch("cb", "?1", "?2", "'publish'")+`
				AND cb.vendor_version = `+profileField("?2", "vendor_version")+`), 0) < ?4,
			(`+CapabilityCovered("?1", "?2", "'turn_end'")+` OR `+CapabilityCovered("?1", printProfile, "'turn_end'")+`)`,
		vendor, profile, builtin, PublishBuildConfirmations).
		Scan(&consent.AutoJoinOff, &consent.PublishCovered, &consent.PublishSwitch, &consent.PublishStopped,
			&consent.PublishProbation, &consent.TurnEndCovered)
}
