package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"modernc.org/sqlite/vfs"
	"net/url"
)

// DoctorAdapterState is the policy and conformance evidence in the store
// for one adapter. The importer reference is deliberately omitted.
type DoctorAdapterState struct {
	Policy             AdapterPolicy
	Evidence           []DoctorEvidence
	UnverifiedLaunches int
}

type DoctorEvidence struct {
	Version     string `json:"version"`
	Platform    string `json:"platform"`
	Mode        string `json:"mode"`
	ManifestRev string `json:"manifest_rev"`
	Capability  string `json:"capability"`
	Level       string `json:"level"`
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
	for _, name := range adapters {
		state, err := readDoctorAdapter(ctx, tx, name)
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

func readDoctorAdapter(ctx context.Context, tx Tx, adapter string) (DoctorAdapterState, error) {
	policy, err := ReadAdapterPolicy(ctx, tx, adapter)
	if err != nil {
		return DoctorAdapterState{}, err
	}
	state := DoctorAdapterState{Policy: policy}
	err = tx.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM adapter_launch l
		WHERE l.ended_hook_at_ms IS NULL AND json_extract(l.profile, '$.adapter') = ?
		  AND l.rowid = (SELECT rowid FROM adapter_launch
		                  WHERE principal_id = l.principal_id
		                  ORDER BY started_hook_at_ms DESC, rowid DESC LIMIT 1)
		  AND EXISTS (SELECT 1 FROM binding WHERE principal_id = l.principal_id AND state = 'live')
		  AND (json_extract(l.profile, '$.vendor_version') = 'unverified'
		       OR json_extract(l.profile, '$.mode') = 'unverified')`, adapter).Scan(&state.UnverifiedLaunches)
	if err != nil {
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
