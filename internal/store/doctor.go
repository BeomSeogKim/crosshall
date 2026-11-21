package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"time"

	"modernc.org/sqlite/vfs"
)

// DoctorAdapterState is the policy and conformance evidence in the store
// for one adapter. The importer reference is deliberately omitted.
type DoctorAdapterState struct {
	Policy             AdapterPolicy
	Evidence           []DoctorEvidence
	UnverifiedLaunches int
	TurnEndNotice      DoctorTurnEndNotice
	Publish            DoctorPublishState
}

// DoctorPublishState reports registered publish routes for one adapter.
type DoctorPublishState struct {
	State        string `json:"state"`
	Reason       string `json:"reason"`
	ActiveRoutes int    `json:"active_routes"`
	TotalRoutes  int    `json:"total_routes"`
	// Counters count the notices reserved on the diagnostics day.
	Counters DoctorPublishCounters `json:"counters"`
	Members  []DoctorPublishMember `json:"members,omitempty"`
	Builds   []DoctorPublishBuild  `json:"builds,omitempty"`
}

// DoctorPublishCounters counts notices by outcome and by observation.
type DoctorPublishCounters struct {
	Date         string `json:"date"`
	Submitted    int    `json:"submitted"`
	Uncertain    int    `json:"uncertain"`
	NotSubmitted int    `json:"not_submitted"`
	OwnTurn      int    `json:"own_turn"`
	Absorbed     int    `json:"absorbed"`
	Unknown      int    `json:"unknown"`
}

// DoctorPublishMember is one live member's publish state.
type DoctorPublishMember struct {
	Channel   string       `json:"channel"`
	Alias     string       `json:"alias"`
	BindingID int64        `json:"binding_id"`
	State     PublishState `json:"publish"`
}

type DoctorPublishBuild struct {
	Version        string `json:"version"`
	Platform       string `json:"platform"`
	Mode           string `json:"mode"`
	ManifestRev    string `json:"manifest_rev"`
	State          string `json:"state"`
	Confirmed      int    `json:"confirmed,omitempty"`
	HeldUnobserved *int   `json:"held_unobserved,omitempty"`
	StoppedAtMS    int64  `json:"stopped_at_ms,omitempty"`
	Notice         int64  `json:"notice,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

type DoctorEvidence struct {
	Version     string `json:"version"`
	Platform    string `json:"platform"`
	Mode        string `json:"mode"`
	ManifestRev string `json:"manifest_rev"`
	Capability  string `json:"capability"`
	Level       string `json:"level"`
}

// DoctorTurnEndNotice reports live eligibility and durable claims for one UTC day.
type DoctorTurnEndNotice struct {
	Available                         bool   `json:"available"`
	TrustedFrom                       string `json:"trusted_from,omitempty"`
	Date                              string `json:"date"`
	CommittedClaims                   int    `json:"committed_claims"`
	ExtraModelRequestsPerContinuation int    `json:"extra_model_requests_per_continuation"`
}

// DoctorSnapshot contains the diagnostics read in one store transaction.
type DoctorSnapshot struct {
	SchemaVersion    int
	Adapters         map[string]DoctorAdapterState
	IdentityVersions []string
}

// InspectDoctorStore reads an existing store without creating or modifying
// any database file. A nonempty identityVendor also requests the distinct
// session versions recorded for that vendor.
func InspectDoctorStore(ctx context.Context, files fs.FS, adapters []string, identityVendor string) (DoctorSnapshot, error) {
	return InspectDoctorStoreAt(ctx, files, adapters, identityVendor, time.Now())
}

// InspectDoctorStoreAt uses the caller's clock for the UTC diagnostics day.
func InspectDoctorStoreAt(ctx context.Context, files fs.FS, adapters []string, identityVendor string, now time.Time) (DoctorSnapshot, error) {
	var snapshot DoctorSnapshot
	// SQLite can interpret a failed sidecar lookup as absence. Check the
	// database and sidecars before opening the diagnostic view.
	for _, name := range []string{"store.sqlite3", "store.sqlite3-wal", "store.sqlite3-journal"} {
		file, err := files.Open(name)
		if err != nil {
			if name != "store.sqlite3" && errors.Is(err, fs.ErrNotExist) {
				continue
			}
			return snapshot, err
		}
		file.Close()
	}
	vfsName, readOnly, err := vfs.New(files)
	if err != nil {
		return snapshot, err
	}
	defer readOnly.Close()
	// Exclusive mode keeps the WAL index in memory. The read-only VFS has no
	// host locking or write paths, so SQLite cannot create or change sidecars.
	query := url.Values{"vfs": {vfsName}, "mode": {"ro"}, "_pragma": {"locking_mode(EXCLUSIVE)"}}
	// After a complete checkpoint, the main file still indicates WAL mode but
	// there is no WAL to read. Immutable mode lets SQLite inspect that file
	// without trying to open a WAL.
	if _, err := fs.Stat(files, "store.sqlite3-wal"); errors.Is(err, fs.ErrNotExist) {
		if _, err := fs.Stat(files, "store.sqlite3-journal"); errors.Is(err, fs.ErrNotExist) {
			query.Set("immutable", "1")
		}
	}
	db, err := sql.Open("sqlite", "file:store.sqlite3?"+query.Encode())
	if err != nil {
		return snapshot, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return snapshot, err
	}
	defer tx.Rollback()
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&snapshot.SchemaVersion); err != nil {
		return snapshot, err
	}
	if snapshot.SchemaVersion != len(migrations) {
		return snapshot, fmt.Errorf("schema version %d, expected %d", snapshot.SchemaVersion, len(migrations))
	}
	snapshot.Adapters = make(map[string]DoctorAdapterState, len(adapters))
	day := now.UTC().Truncate(24 * time.Hour)
	for _, name := range adapters {
		state, err := readDoctorAdapter(ctx, tx, name, day, now)
		if err == nil {
			snapshot.Adapters[name] = state
		}
	}
	if identityVendor != "" {
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT COALESCE(NULLIF(vendor_version, ''), 'unknown') AS version
			FROM principal WHERE kind = 'session' AND vendor = ? ORDER BY version`, identityVendor)
		if err != nil {
			return snapshot, err
		}
		defer rows.Close()
		for rows.Next() {
			var version string
			if err := rows.Scan(&version); err != nil {
				return snapshot, err
			}
			snapshot.IdentityVersions = append(snapshot.IdentityVersions, version)
		}
		return snapshot, rows.Err()
	}
	return snapshot, nil
}

func readDoctorAdapter(ctx context.Context, tx Tx, adapter string, day, now time.Time) (DoctorAdapterState, error) {
	policy, err := ReadAdapterPolicy(ctx, tx, adapter)
	if err != nil {
		return DoctorAdapterState{}, err
	}
	state := DoctorAdapterState{Policy: policy}
	state.TurnEndNotice.Date = day.Format("20060102")
	state.TurnEndNotice.ExtraModelRequestsPerContinuation = 1
	err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM turn_end_notice WHERE adapter = ? AND at_ms >= ? AND at_ms < ?`,
		adapter, day.UnixMilli(), day.AddDate(0, 0, 1).UnixMilli()).Scan(&state.TurnEndNotice.CommittedClaims)
	if err != nil {
		return DoctorAdapterState{}, err
	}
	var activeRoutes, totalRoutes int
	var sampleReason sql.NullString
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(CASE WHEN state = 'active' THEN 1 ELSE 0 END), 0),
		       COUNT(*),
		       MAX(CASE WHEN state = 'active' THEN reason ELSE NULL END)
		FROM publish_route WHERE adapter = ?`, adapter).
		Scan(&activeRoutes, &totalRoutes, &sampleReason); err != nil {
		return DoctorAdapterState{}, err
	}
	state.Publish = DoctorPublishState{
		State:        "candidate",
		Reason:       "slice 1 registry only (no sender)",
		ActiveRoutes: activeRoutes,
		TotalRoutes:  totalRoutes,
	}
	if activeRoutes == 0 {
		state.Publish.Reason = "no active route"
	} else if sampleReason.Valid && sampleReason.String != "" {
		state.Publish.Reason = sampleReason.String
	}
	if err := readDoctorBuilds(ctx, tx, adapter, &state); err != nil {
		return DoctorAdapterState{}, err
	}
	if err := readDoctorPublish(ctx, tx, adapter, day, now, &state.Publish); err != nil {
		return DoctorAdapterState{}, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT vendor_version, platform, mode, manifest_rev, cap, level
		FROM capability_evidence WHERE vendor = ?
		ORDER BY vendor_version, platform, mode, manifest_rev, cap`, adapter)
	if err != nil {
		return DoctorAdapterState{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var e DoctorEvidence
		if err := rows.Scan(&e.Version, &e.Platform, &e.Mode, &e.ManifestRev, &e.Capability, &e.Level); err != nil {
			return DoctorAdapterState{}, err
		}
		state.Evidence = append(state.Evidence, e)
	}
	return state, rows.Err()
}

func readDoctorBuilds(ctx context.Context, tx Tx, adapter string, out *DoctorAdapterState) error {
	rows, err := tx.QueryContext(ctx, `SELECT l.profile, COUNT(*) FROM adapter_launch l
		WHERE l.ended_hook_at_ms IS NULL AND json_extract(l.profile, '$.adapter') = ?
			AND l.rowid = (SELECT current.rowid FROM adapter_launch current
				WHERE current.principal_id = l.principal_id
				ORDER BY current.ended_hook_at_ms IS NOT NULL, current.started_hook_at_ms DESC, current.rowid DESC LIMIT 1)
			AND EXISTS (SELECT 1 FROM binding WHERE principal_id = l.principal_id AND state = 'live')
		GROUP BY l.profile ORDER BY json_extract(l.profile, '$.vendor_version'), json_extract(l.profile, '$.platform'),
			json_extract(l.profile, '$.mode'), json_extract(l.profile, '$.manifest_rev')`, adapter)
	if err != nil {
		return err
	}
	defer rows.Close()
	type profileCount struct {
		profile string
		count   int
	}
	var profiles []profileCount
	for rows.Next() {
		var profile profileCount
		if err := rows.Scan(&profile.profile, &profile.count); err != nil {
			return err
		}
		profiles = append(profiles, profile)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	var exactTurnEnd bool
	for _, profile := range profiles {
		turnEnd, err := readCapabilityBuild(ctx, tx, adapter, profile.profile, "turn_end")
		if err != nil {
			return err
		}
		if turnEnd.Version == "unverified" || turnEnd.Mode == "unverified" {
			out.UnverifiedLaunches += profile.count
			continue
		}
		if out.Policy.Enabled && turnEnd.Covered {
			out.TurnEndNotice.Available = true
			if turnEnd.ExactLevel == "tested" {
				exactTurnEnd = true
			} else if out.TurnEndNotice.TrustedFrom == "" {
				out.TurnEndNotice.TrustedFrom = turnEnd.TestedFrom
			}
		}
		publish, err := readCapabilityBuild(ctx, tx, adapter, profile.profile, "publish")
		if err != nil {
			return err
		}
		build := doctorPublishBuild(publish)
		if build.State == "trusted" {
			var held int
			err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM publish_attempt a
				JOIN adapter_launch l ON l.principal_id = a.principal_id AND l.launch_ref = a.launch_ref
				WHERE a.adapter = ? AND a.observed_at_ms IS NULL AND a.retired_at_ms IS NULL
					AND a.state <> 'not_submitted' AND a.state <> 'reserved'
					AND (json_extract(l.profile, '$.vendor_version'), json_extract(l.profile, '$.platform'),
						json_extract(l.profile, '$.mode'), json_extract(l.profile, '$.manifest_rev')) = (?, ?, ?, ?)`,
				adapter, build.Version, build.Platform, build.Mode, build.ManifestRev).Scan(&held)
			if err != nil {
				return err
			}
			build.HeldUnobserved = &held
		}
		out.Publish.Builds = append(out.Publish.Builds, build)
	}
	if exactTurnEnd {
		out.TurnEndNotice.TrustedFrom = ""
	}
	return nil
}

func doctorPublishBuild(build capabilityBuildEvidence) DoctorPublishBuild {
	out := DoctorPublishBuild{Version: build.Version, Platform: build.Platform, Mode: build.Mode, ManifestRev: build.ManifestRev}
	switch {
	case build.Stopped:
		out.State, out.StoppedAtMS, out.Notice, out.Reason = "stopped", build.StoppedAtMS, build.Notice, build.StopReason
	case !build.Covered:
		out.State, out.Reason = "candidate", build.candidateReason()
	case build.ExactLevel == "tested":
		out.State = "tested"
	case build.Confirmed == PublishBuildConfirmations:
		out.State = "seen"
	default:
		out.State, out.Confirmed = "trusted", build.Confirmed
	}
	return out
}

func readDoctorPublish(ctx context.Context, tx Tx, adapter string, day, now time.Time, out *DoctorPublishState) error {
	c := &out.Counters
	c.Date = day.Format("20060102")
	if err := tx.QueryRowContext(ctx, `SELECT
			COALESCE(SUM(state = 'submitted'), 0), COALESCE(SUM(state = 'uncertain'), 0),
			COALESCE(SUM(state = 'not_submitted'), 0), COALESCE(SUM(observation = 'own_turn'), 0),
			COALESCE(SUM(observation = 'absorbed'), 0), COALESCE(SUM(observation = 'unknown'), 0)
		FROM publish_attempt WHERE adapter = ? AND reserved_at_ms >= ? AND reserved_at_ms < ?`,
		adapter, day.UnixMilli(), day.AddDate(0, 0, 1).UnixMilli()).
		Scan(&c.Submitted, &c.Uncertain, &c.NotSubmitted, &c.OwnTurn, &c.Absorbed, &c.Unknown); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT c.name, b.alias, b.id FROM publish_route r
		JOIN binding b ON b.id = r.binding_id JOIN channel c ON c.id = b.channel_id
		WHERE r.adapter = ? AND b.state = 'live' ORDER BY c.name, b.alias`, adapter)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var m DoctorPublishMember
		if err := rows.Scan(&m.Channel, &m.Alias, &m.BindingID); err != nil {
			return err
		}
		out.Members = append(out.Members, m)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range out.Members {
		if out.Members[i].State, _, err = ReadPublishState(ctx, tx, out.Members[i].BindingID, now); err != nil {
			return err
		}
	}
	return nil
}
