package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"

	"modernc.org/sqlite"
)

//go:embed migrations/001_init.sql
var initialSchema string

//go:embed migrations/002_adapters.sql
var adapterSchema string

//go:embed migrations/003_owner_depart.sql
var ownerDepartSchema string

//go:embed migrations/004_turn_end_notice.sql
var turnEndNoticeSchema string

//go:embed migrations/005_join_pool.sql
var joinPoolSchema string

//go:embed migrations/006_linked_handoff.sql
var linkedHandoffSchema string

//go:embed migrations/007_publish_routes.sql
var publishRoutesSchema string

//go:embed migrations/008_publish_attempts.sql
var publishAttemptsSchema string

//go:embed migrations/009_root_refill.sql
var rootRefillSchema string

//go:embed migrations/010_publish_switches.sql
var publishSwitchesSchema string

//go:embed migrations/011_publish_activation.sql
var publishActivationSchema string

//go:embed migrations/012_auto_join.sql
var autoJoinSchema string

//go:embed migrations/013_start_line.sql
var startLineSchema string

//go:embed migrations/014_cleared_keys.sql
var clearedKeysSchema string

//go:embed migrations/015_launch_process.sql
var launchProcessSchema string

//go:embed migrations/016_capability_build.sql
var capabilityBuildSchema string

//go:embed migrations/017_route_socket_attach.sql
var routeSocketAttachSchema string

//go:embed migrations/018_turn_end_switch.sql
var turnEndSwitchSchema string

var migrations = []string{
	initialSchema, adapterSchema, ownerDepartSchema, turnEndNoticeSchema,
	joinPoolSchema, linkedHandoffSchema, publishRoutesSchema, publishAttemptsSchema, rootRefillSchema,
	publishSwitchesSchema, publishActivationSchema, autoJoinSchema, startLineSchema, clearedKeysSchema, launchProcessSchema,
	capabilityBuildSchema, routeSocketAttachSchema, turnEndSwitchSchema,
}

// Store owns the dispatcher's single SQLite connection.
type Store struct {
	db *sql.DB
}

// Open configures and migrates the store after the caller takes the dispatcher
// lock. stateRoot must already be an existing, private directory.
func Open(ctx context.Context, stateRoot string) (*Store, error) {
	if !filepath.IsAbs(stateRoot) {
		return nil, errors.New("store: state root must be absolute")
	}
	path := filepath.Join(stateRoot, "store.sqlite3")
	previous, err := prepareFile(path)
	if err != nil {
		return nil, err
	}
	pragmas := url.Values{"_txlock": {"immediate"}, "_pragma": {"synchronous(FULL)", "foreign_keys(ON)"}}
	if runtime.GOOS == "darwin" {
		pragmas.Add("_pragma", "fullfsync(ON)")
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: pragmas.Encode()}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	s := &Store{db: db}
	if err := s.initialize(ctx, stateRoot, previous); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func prepareFile(path string) (bool, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
		if err != nil {
			return false, err
		}
		return false, file.Close()
	}
	if err != nil {
		return false, err
	}
	if fi.Mode() != 0600 {
		return false, fmt.Errorf("store: %s must be a regular 0600 file", path)
	}
	return fi.Size() != 0, nil
}

func (s *Store) initialize(ctx context.Context, stateRoot string, previous bool) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version < 0 || version > len(migrations) {
		return fmt.Errorf("store: unsupported schema version %d (supported: 0–%d)", version, len(migrations))
	}
	var mode string
	if err := s.db.QueryRowContext(ctx, "PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		return err
	}
	if mode != "wal" {
		return fmt.Errorf("store: expected WAL, got %q", mode)
	}
	for pragma, want := range map[string]int{"foreign_keys": 1, "synchronous": 2, "fullfsync": 1} {
		if pragma == "fullfsync" && runtime.GOOS != "darwin" {
			continue
		}
		var got int
		if err := s.db.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("store: PRAGMA %s is %d, want %d", pragma, got, want)
		}
	}
	if version == len(migrations) {
		return nil
	}
	var backupPath string
	if previous {
		var err error
		backupPath, err = s.backup(ctx, stateRoot, version)
		if err != nil {
			return fmt.Errorf("store: back up schema %d: %w", version, err)
		}
	}
	for step := version; step < len(migrations); step++ {
		err := s.Update(ctx, func(tx Tx) error {
			if _, err := tx.ExecContext(ctx, migrations[step]); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version=%d", step+1))
			return err
		})
		if err != nil {
			if backupPath != "" {
				return fmt.Errorf("store: migrate to v%d (backup %s): %w", step+1, backupPath, err)
			}
			return fmt.Errorf("store: migrate to v%d: %w", step+1, err)
		}
	}
	return nil
}

func (s *Store) backup(ctx context.Context, stateRoot string, version int) (string, error) {
	file, err := os.CreateTemp(stateRoot, fmt.Sprintf("store-v%d-*.sqlite3", version))
	if err != nil {
		return "", err
	}
	path := file.Name()
	err = file.Close()
	if err == nil {
		err = s.copyTo(ctx, path)
	}
	if err != nil {
		os.Remove(path)
		return "", err
	}
	return path, nil
}

func (s *Store) copyTo(ctx context.Context, path string) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.Raw(func(driver any) error {
		uri := (&url.URL{Scheme: "file", Path: path}).String()
		backup, err := driver.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		}).NewBackup(uri)
		if err != nil {
			return err
		}
		for {
			if err := ctx.Err(); err != nil {
				return errors.Join(err, backup.Finish())
			}
			more, err := backup.Step(128)
			if err != nil || !more {
				return errors.Join(err, backup.Finish())
			}
		}
	})
}

// CommitError keeps failures during commit distinct from failures in apply.
type CommitError struct{ err error }

func (e *CommitError) Error() string { return "store: commit: " + e.err.Error() }
func (e *CommitError) Unwrap() error { return e.err }

// Update runs apply in one write transaction and rolls back if apply fails.
func (s *Store) Update(ctx context.Context, apply func(Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := apply(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return &CommitError{err}
	}
	return nil
}

// Close closes the dispatcher's database connection.
func (s *Store) Close() error { return s.db.Close() }
