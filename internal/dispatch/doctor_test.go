package dispatch

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func TestDoctorInspectsLiveStoreWithoutSideEffects(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, _ := doctorFixture(t)
	database := filepath.Join(cfg.StateRoot, "store.sqlite3")
	mainBefore := readDoctorFile(t, database)
	if version := binary.BigEndian.Uint32(mainBefore[60:64]); version != 0 {
		t.Fatalf("fixture schema was checkpointed: version %d", version)
	}
	walBefore := readDoctorFile(t, database+"-wal")
	shmBefore := readDoctorFile(t, database+"-shm")
	checkDoctor(t, cfg.Doctor(t.Context()), core.ExitOK, true, true, true, true, true)
	for _, snapshot := range []struct {
		path   string
		before []byte
	}{{database, mainBefore}, {database + "-wal", walBefore}, {database + "-shm", shmBefore}} {
		if after := readDoctorFile(t, snapshot.path); !bytes.Equal(after, snapshot.before) {
			t.Errorf("doctor changed %s", filepath.Base(snapshot.path))
		}
	}
	entries, err := os.ReadDir(filepath.Join(cfg.StateRoot, "owner", "in"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("owner spool after doctor: %v, %v", entries, err)
	}
}

func TestDoctorReportsAbsentAndStaleDispatcher(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Run("absent", func(t *testing.T) {
		dir := t.TempDir()
		cfg := LaunchConfig{Version: "test", StateRoot: filepath.Join(dir, "state"), Rendezvous: filepath.Join(dir, "rendezvous")}
		checkDoctor(t, cfg.Doctor(t.Context()), core.ExitError, false, false, false, false, false)
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) != 0 {
			t.Fatalf("parent directory after doctor: %v, %v", entries, err)
		}
	})
	t.Run("stale", func(t *testing.T) {
		cfg, lock := doctorFixture(t)
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
			t.Fatal(err)
		}
		checkDoctor(t, cfg.Doctor(t.Context()), core.ExitError, false, false, true, true, true)
	})
}

func TestDoctorInspectsCheckpointedStoreWithoutCreatingSidecars(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	state := t.TempDir()
	if err := os.Chmod(state, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), state)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	cfg := LaunchConfig{Version: "test", StateRoot: state, Rendezvous: filepath.Join(t.TempDir(), "absent")}
	checkDoctor(t, cfg.Doctor(t.Context()), core.ExitError, false, false, true, false, false)
	files, err := filepath.Glob(filepath.Join(state, "*"))
	if want := []string{filepath.Join(state, "store.sqlite3")}; err != nil || !slices.Equal(files, want) {
		t.Fatalf("state after doctor: %v, %v; want %v", files, err, want)
	}
}

func TestDoctorRejectsUnsafePaths(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Run("unwritable spool", func(t *testing.T) {
		cfg, _ := doctorFixture(t)
		if err := os.Chmod(filepath.Join(cfg.StateRoot, "owner", "in"), 0500); err != nil {
			t.Fatal(err)
		}
		checkDoctor(t, cfg.Doctor(t.Context()), core.ExitError, true, true, true, true, false)
	})
	t.Run("spool symlink", func(t *testing.T) {
		cfg, _ := doctorFixture(t)
		in := filepath.Join(cfg.StateRoot, "owner", "in")
		if err := os.Remove(in); err != nil {
			t.Fatal(err)
		}
		target := t.TempDir()
		if err := os.Symlink(target, in); err != nil {
			t.Fatal(err)
		}
		checkDoctor(t, cfg.Doctor(t.Context()), core.ExitError, true, true, true, true, false)
		entries, err := os.ReadDir(target)
		if err != nil || len(entries) != 0 {
			t.Fatalf("symlink target after doctor: %v, %v", entries, err)
		}
	})
	t.Run("insecure rendezvous", func(t *testing.T) {
		cfg, _ := doctorFixture(t)
		if err := os.Chmod(cfg.Rendezvous, 0755); err != nil {
			t.Fatal(err)
		}
		checkDoctor(t, cfg.Doctor(t.Context()), core.ExitError, true, false, true, false, true)
	})
}

func doctorFixture(t *testing.T) (LaunchConfig, *os.File) {
	t.Helper()
	cfg := LaunchConfig{Version: "test", StateRoot: t.TempDir(), Rendezvous: t.TempDir(), Owner: true}
	for _, root := range []string{cfg.StateRoot, cfg.Rendezvous} {
		if err := os.Chmod(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	state, err := spool.OpenServerRoot(cfg.StateRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	lock, err := spool.OpenServerFile(state, "dispatcher.lock", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Close() })
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), cfg.StateRoot)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if err := prepareNamedSpool(state, "owner"); err != nil {
		t.Fatal(err)
	}
	rendezvous, err := spool.OpenServerRoot(cfg.Rendezvous)
	if err != nil {
		t.Fatal(err)
	}
	defer rendezvous.Close()
	marker := readyMarker{Instance: "doctor-test", Version: wire.Version, PID: os.Getpid()}
	if err := spool.PublishServerFile(rendezvous, "ready", marker); err != nil {
		t.Fatal(err)
	}
	return cfg, lock
}

func checkDoctor(t *testing.T, result core.Result, status core.ExitCode, want ...bool) {
	t.Helper()
	if result.Status != status {
		t.Errorf("status = %d, want %d (%s)", result.Status, status, result.Data)
	}
	var data struct {
		Checks []doctorCheck `json:"checks"`
	}
	names := []string{"lock", "readiness", "store_version", "rendezvous_permissions", "owner_spool_writable"}
	if err := json.Unmarshal(result.Data, &data); err != nil || len(data.Checks) != len(names) {
		t.Fatalf("checks = %s, decode error = %v", result.Data, err)
	}
	for i, check := range data.Checks {
		if check.Name != names[i] || check.OK != want[i] || check.Detail == "" {
			t.Errorf("check %d = %+v, want %s ok=%v with a detail", i, check, names[i], want[i])
		}
	}
}

func readDoctorFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestDoctorNamesTheStartCommandWhenNoDispatcherRuns(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	hint := func(r core.Result) string {
		var data struct {
			Hint string `json:"hint"`
		}
		if err := json.Unmarshal(r.Data, &data); err != nil {
			t.Fatal(err)
		}
		return data.Hint
	}
	dir := t.TempDir()
	absent := LaunchConfig{Version: "test", StateRoot: filepath.Join(dir, "state"), Rendezvous: filepath.Join(dir, "rendezvous")}
	if got := hint(absent.Doctor(t.Context())); got != "crosshall owner start" {
		t.Errorf("never started: hint %q", got)
	}
	stale, lock := doctorFixture(t)
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if got := hint(stale.Doctor(t.Context())); got != "crosshall owner start" {
		t.Errorf("stopped: hint %q", got)
	}
	live, _ := doctorFixture(t)
	if got := hint(live.Doctor(t.Context())); got != "" {
		t.Errorf("running: hint %q", got)
	}
	insecure, _ := doctorFixture(t)
	if err := os.Chmod(insecure.Rendezvous, 0755); err != nil {
		t.Fatal(err)
	}
	if got := hint(insecure.Doctor(t.Context())); got != "" {
		t.Errorf("running with an unsafe rendezvous: hint %q", got)
	}
}

func TestDoctorDoesNotRecommendStartWhenTheLockCannotBeInspected(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, _ := doctorFixture(t)
	if err := os.Chmod(filepath.Join(cfg.StateRoot, "dispatcher.lock"), 0400); err != nil {
		t.Fatal(err)
	}
	result := cfg.Doctor(t.Context())
	var data struct {
		Checks []doctorCheck `json:"checks"`
		Hint   string        `json:"hint"`
	}
	if err := json.Unmarshal(result.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Hint != "" {
		t.Errorf("a held lock that cannot be inspected gets the start hint %q", data.Hint)
	}
	if lock := data.Checks[0]; lock.OK || !strings.Contains(lock.Detail, "cannot be inspected") || strings.Contains(lock.Detail, "idle") {
		t.Errorf("lock check %+v, want the inspection error", lock)
	}
}

func TestDoctorRecommendsStartWhenTheLockFileIsMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg, lock := doctorFixture(t)
	lock.Close()
	if err := os.Remove(filepath.Join(cfg.StateRoot, "dispatcher.lock")); err != nil {
		t.Fatal(err)
	}
	var data struct {
		Checks []doctorCheck `json:"checks"`
		Hint   string        `json:"hint"`
	}
	if err := json.Unmarshal(cfg.Doctor(t.Context()).Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Hint != "crosshall owner start" || data.Checks[0].Detail != "dispatcher lock is absent or idle" {
		t.Errorf("missing lock: hint %q, lock check %+v", data.Hint, data.Checks[0])
	}
}
