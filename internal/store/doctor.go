package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"net/url"

	"modernc.org/sqlite/vfs"
)

// CheckVersion inspects an existing store without opening its files for writing.
func CheckVersion(ctx context.Context, files fs.FS) (int, error) {
	vfsName, readOnly, err := vfs.New(files)
	if err != nil {
		return 0, err
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
		return 0, err
	}
	defer db.Close()
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return 0, err
	}
	if version != len(migrations) {
		return version, fmt.Errorf("schema version %d, expected %d", version, len(migrations))
	}
	return version, nil
}
