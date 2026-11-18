package store

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreRollbackAndReopen(t *testing.T) {
	root := t.TempDir()
	ctx := t.Context()
	db, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertChannel(ctx, db, "committed", nil); err != nil {
		t.Fatal(err)
	}
	refusal := errors.New("rollback")
	if err := insertChannel(ctx, db, "discarded", refusal); !errors.Is(err, refusal) {
		t.Fatalf("failed to roll back: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Update(ctx, func(tx Tx) error {
		var version, count int
		var channel string
		if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, "SELECT count(*), min(name) FROM channel").Scan(&count, &channel); err != nil {
			return err
		}
		if version != 1 || count != 1 || channel != "committed" {
			t.Fatalf("schema=%d rows=%d channel=%q", version, count, channel)
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO repo_default VALUES ('/test', 99)"); err == nil {
			t.Fatal("foreign keys did not reject the unknown channel")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func insertChannel(ctx context.Context, db *Store, name string, failure error) error {
	return db.Update(ctx, func(tx Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO channel(name, created_at_ms) VALUES (?, 1)", name)
		return errors.Join(err, failure)
	})
}

func TestMigrationRollbackKeepsBackup(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "state?dev")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := Open(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	if err := insertChannel(t.Context(), db, "existing", nil); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	saved := migrations
	migrations = []string{initialSchema, "CREATE TABLE discarded (id INTEGER); THIS IS NOT SQL;"}
	t.Cleanup(func() { migrations = saved })
	_, err = Open(t.Context(), root)
	if err == nil || !strings.Contains(err.Error(), "backup ") {
		t.Fatalf("migration error did not report its backup: %v", err)
	}
	migrations = saved
	backup, err := filepath.Glob(filepath.Join(root, "store-v1-*.sqlite3"))
	if err != nil || len(backup) != 1 {
		t.Fatalf("backup files: %v, error: %v", backup, err)
	}
	for _, path := range []string{filepath.Join(root, "store.sqlite3"), backup[0]} {
		checkOriginalSchema(t, path)
	}
	if _, err := os.Lstat(filepath.Join(parent, "state")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backup must leave the sibling path untouched: %v", err)
	}
}

func checkOriginalSchema(t *testing.T, path string) {
	t.Helper()
	uri := (&url.URL{Scheme: "file", Path: path}).String()
	db, err := sql.Open("sqlite", uri)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for query, want := range map[string]int{
		"PRAGMA user_version": 1,
		"SELECT count(*) FROM channel WHERE name = 'existing'":        1,
		"SELECT count(*) FROM sqlite_schema WHERE name = 'discarded'": 0,
	} {
		var read int
		if err := db.QueryRowContext(t.Context(), query).Scan(&read); err != nil || read != want {
			t.Fatalf("%s: %s = %d, want %d, error: %v", path, query, read, want, err)
		}
	}
}
