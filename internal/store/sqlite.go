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
	"slices"
	"sync"

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

//go:embed migrations/019_reach_told.sql
var reachToldSchema string

//go:embed migrations/020_member_host.sql
var memberHostSchema string

//go:embed migrations/021_loop_guard.sql
var loopGuardSchema string

//go:embed migrations/022_loop_guard_cutover.sql
var loopGuardCutoverSchema string

//go:embed migrations/023_publish_recovery_hint.sql
var publishRecoveryHintSchema string

//go:embed migrations/024_owner_alert.sql
var ownerAlertSchema string

//go:embed migrations/025_thread_lanes.sql
var threadLanesSchema string

//go:embed migrations/026_unknown_receipt_cause.sql
var unknownReceiptCauseSchema string

//go:embed migrations/027_offer_receipts.sql
var offerReceiptsSchema string

//go:embed migrations/028_session_move.sql
var sessionMoveSchema string

//go:embed migrations/029_idle_prompt.sql
var idlePromptSchema string

//go:embed migrations/030_reconcile_checkpoint.sql
var reconcileCheckpointSchema string

//go:embed migrations/031_link_devices.sql
var linkDevicesSchema string

//go:embed migrations/032_device_members.sql
var deviceMembersSchema string

//go:embed migrations/033_link_fences.sql
var linkFencesSchema string

//go:embed migrations/034_link_events.sql
var linkEventsSchema string

//go:embed migrations/035_link_wakes.sql
var linkWakesSchema string

//go:embed link_wake_triggers.sql
var linkWakeTriggers string

//go:embed link_offer_triggers.sql
var linkOfferTriggers string

//go:embed migrations/spoke/001_spoke.sql
var spokeSchema string

//go:embed migrations/spoke/002_retry.sql
var spokeRetrySchema string

// spokeMigrations is the schema of a linked device's spoke store.
var spokeMigrations = []string{spokeSchema, spokeRetrySchema}

var migrations = []string{
	initialSchema, adapterSchema, ownerDepartSchema, turnEndNoticeSchema,
	joinPoolSchema, linkedHandoffSchema, publishRoutesSchema, publishAttemptsSchema, rootRefillSchema,
	publishSwitchesSchema, publishActivationSchema, autoJoinSchema, startLineSchema, clearedKeysSchema, launchProcessSchema,
	capabilityBuildSchema, routeSocketAttachSchema, turnEndSwitchSchema, reachToldSchema, memberHostSchema, loopGuardSchema,
	loopGuardCutoverSchema, publishRecoveryHintSchema, ownerAlertSchema, threadLanesSchema,
	unknownReceiptCauseSchema, offerReceiptsSchema, sessionMoveSchema, idlePromptSchema, reconcileCheckpointSchema,
	linkDevicesSchema, deviceMembersSchema, linkFencesSchema, linkEventsSchema, linkWakesSchema,
}

// Store owns the dispatcher's single SQLite write connection, and opens a
// read-only one for long reads on first use.
type Store struct {
	schema  []string
	db      *sql.DB
	readDSN string
	readMu  sync.Mutex
	reader  *sql.DB
}

// Open configures and migrates the store after the caller takes the dispatcher
// lock. stateRoot must already be an existing, private directory.
func Open(ctx context.Context, stateRoot string) (*Store, error) {
	return open(ctx, stateRoot, migrations)
}

// OpenSpoke opens the spoke store of a linked device in dir, an existing
// private directory, with the same durability as the hall's store.
func OpenSpoke(ctx context.Context, dir string) (*Store, error) {
	return open(ctx, dir, spokeMigrations)
}

func open(ctx context.Context, stateRoot string, schema []string) (*Store, error) {
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
	read := url.Values{"_txlock": {"deferred"}, "_pragma": {"query_only(ON)", "busy_timeout(5000)"}}
	s := &Store{schema: schema, db: db, readDSN: (&url.URL{Scheme: "file", Path: path, RawQuery: read.Encode()}).String()}
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
	if version < 0 || version > len(s.schema) {
		return fmt.Errorf("store: unsupported schema version %d (supported: 0–%d)", version, len(s.schema))
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
	if version == len(s.schema) {
		return s.ensureReadIndexes(ctx)
	}
	var backupPath string
	if previous {
		var err error
		backupPath, err = s.backup(ctx, stateRoot, version)
		if err != nil {
			return fmt.Errorf("store: back up schema %d: %w", version, err)
		}
	}
	for step := version; step < len(s.schema); step++ {
		err := s.Update(ctx, func(tx Tx) error {
			if s.schema[step] == sessionMoveSchema {
				if err := checkSessionMoveBindings(ctx, tx); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, s.schema[step]); err != nil {
				return err
			}
			var installLinkedTriggers func(context.Context, Tx) error
			switch s.schema[step] {
			case linkEventsSchema:
				installLinkedTriggers = InstallLinkOfferTriggers
			case linkWakesSchema:
				installLinkedTriggers = InstallLinkWakeTriggers
			}
			if installLinkedTriggers != nil {
				var enrolled bool
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM device)`).Scan(&enrolled); err != nil {
					return err
				}
				if enrolled {
					if err := installLinkedTriggers(ctx, tx); err != nil {
						return err
					}
				}
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
	return s.ensureReadIndexes(ctx)
}

// ReadIndexes let the latest fact and a launch's turns be read from the
// newest rows instead of all of a launch's observations (#366). They are not
// part of a schema version: an index changes no result, and a binary that
// does not know them keeps them current on its writes, so going back to it
// needs no schema step. A migration that wants one of these names must use
// IF NOT EXISTS.
const ReadIndexes = `
CREATE INDEX IF NOT EXISTS observation_launch_hook ON observation(principal_id, launch_ref, hook_at_ms);
CREATE INDEX IF NOT EXISTS observation_launch_turn ON observation(principal_id, launch_ref, turn_ref);
CREATE INDEX IF NOT EXISTS turn_launch_opened ON turn(principal_id, launch_ref, opened_hook_at_ms);`

func (s *Store) ensureReadIndexes(ctx context.Context) error {
	if !slices.Contains(s.schema, adapterSchema) {
		return nil // a schema without launches or turns
	}
	return s.Update(ctx, func(tx Tx) error {
		_, err := tx.ExecContext(ctx, ReadIndexes)
		return err
	})
}

// InstallLinkOfferTriggers enables remote offer invalidation only in halls
// with enrolled devices. Migration 034 calls it for existing enrollments.
func InstallLinkOfferTriggers(ctx context.Context, tx Tx) error {
	_, err := tx.ExecContext(ctx, linkOfferTriggers)
	return err
}

// InstallLinkWakeTriggers records scope supersession in the writer that
// changes it. Only enrolled halls install these triggers.
func InstallLinkWakeTriggers(ctx context.Context, tx Tx) error {
	_, err := tx.ExecContext(ctx, linkWakeTriggers)
	return err
}

func checkSessionMoveBindings(ctx context.Context, tx Tx) error {
	var principal int64
	var bindings string
	err := tx.QueryRowContext(ctx, `SELECT principal_id, GROUP_CONCAT(id, ', ' ORDER BY id) FROM binding
		GROUP BY principal_id HAVING COUNT(*) > 1 ORDER BY principal_id LIMIT 1`).Scan(&principal, &bindings)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("cannot select a membership for principal %d with bindings %s", principal, bindings)
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
func (s *Store) Close() error {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	var err error
	if s.reader != nil {
		err = s.reader.Close()
	}
	return errors.Join(s.db.Close(), err)
}

// View runs read in a read transaction on a connection of its own. In WAL
// mode it sees one snapshot and neither waits for nor holds the write
// connection, so a long read does not delay the dispatcher's requests.
func (s *Store) View(ctx context.Context, read func(Tx) error) error {
	s.readMu.Lock()
	if s.reader == nil {
		reader, err := sql.Open("sqlite", s.readDSN)
		if err != nil {
			s.readMu.Unlock()
			return err
		}
		reader.SetMaxOpenConns(1)
		reader.SetMaxIdleConns(1)
		s.reader = reader
	}
	reader := s.reader
	s.readMu.Unlock()
	tx, err := reader.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return read(tx)
}
