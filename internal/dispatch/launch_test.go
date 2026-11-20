package dispatch

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func TestPermissionDeniedProbeRecognizesRunningDispatcher(t *testing.T) {
	cfg, _ := doctorFixture(t)
	state, err := spool.OpenServerRoot(cfg.StateRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	marker := readReady(cfg.Rendezvous)
	// A linked log makes a startup attempt observable without launching a child.
	if err := os.Symlink("unused", filepath.Join(cfg.StateRoot, "dispatcher.log")); err != nil {
		t.Fatal(err)
	}
	for _, probeErr := range []syscall.Errno{syscall.EPERM, syscall.EACCES} {
		t.Run(probeErr.Error(), func(t *testing.T) {
			probe := func(pid int, signal syscall.Signal) error {
				if pid != marker.PID || signal != 0 {
					t.Fatalf("unexpected liveness probe: %d, %d", pid, signal)
				}
				return probeErr
			}
			if running, err := marker.liveness(state, probe); !running || err != nil {
				t.Errorf("liveness: running=%t, err=%v", running, err)
			}
			if spawned, err := cfg.start(t.Context(), probe); spawned || err != nil {
				t.Errorf("owner start: spawned=%t, err=%v", spawned, err)
			}
		})
	}
}

func TestMemberStartWithRestrictedLiveness(t *testing.T) {
	for _, tc := range []struct {
		name      string
		lockMode  os.FileMode
		lockHeld  bool
		probeErr  error
		wantStart bool
	}{
		{name: "signal permission denied", lockMode: 0600, lockHeld: true, probeErr: syscall.EPERM},
		{name: "signal access denied", lockMode: 0600, lockHeld: true, probeErr: syscall.EACCES},
		{name: "lock cannot be inspected", lockMode: 0644},
		{name: "signal inconclusive", lockMode: 0600, lockHeld: true, probeErr: syscall.ENOSYS},
		{name: "dispatcher lock released", lockMode: 0600, probeErr: syscall.EPERM, wantStart: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			cfg := LaunchConfig{Version: "test", StateRoot: filepath.Join(root, "state"), Rendezvous: filepath.Join(root, "rendezvous")}
			for _, dir := range []string{cfg.StateRoot, cfg.Rendezvous} {
				if err := os.Mkdir(dir, 0700); err != nil {
					t.Fatal(err)
				}
			}
			lock, err := os.OpenFile(filepath.Join(cfg.StateRoot, "dispatcher.lock"), os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if err := lock.Chmod(tc.lockMode); err != nil {
				t.Fatal(err)
			}
			if tc.lockHeld {
				if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					t.Fatal(err)
				}
			}
			rv, err := spool.OpenServerRoot(cfg.Rendezvous)
			if err != nil {
				t.Fatal(err)
			}
			defer rv.Close()
			marker := readyMarker{Instance: "test-instance", Version: wire.Version, PID: os.Getpid()}
			if err := spool.PublishServerFile(rv, "ready", marker); err != nil {
				t.Fatal(err)
			}
			// A linked log makes a startup attempt observable without launching a child.
			if err := os.Symlink("unused", filepath.Join(cfg.StateRoot, "dispatcher.log")); err != nil {
				t.Fatal(err)
			}
			spawned, err := cfg.start(t.Context(), func(pid int, signal syscall.Signal) error {
				if pid != marker.PID || signal != 0 {
					t.Fatalf("unexpected liveness probe: %d, %d", pid, signal)
				}
				return tc.probeErr
			})
			startAttempted := err != nil && strings.Contains(err.Error(), "dispatcher.log")
			if spawned || startAttempted != tc.wantStart || (!tc.wantStart && err != nil) {
				t.Fatalf("spawned=%t, start attempt wanted=%t, error=%v", spawned, tc.wantStart, err)
			}
		})
	}
}

// A child that exits before it is ready ends the wait at once instead of
// after the startup timeout.
func TestStartEndsWhenTheDispatcherExitsBeforeItIsReady(t *testing.T) {
	root := t.TempDir()
	cfg := LaunchConfig{Version: "test", StateRoot: filepath.Join(root, "state"), Rendezvous: filepath.Join(root, "rendezvous"),
		DefaultStateRoot: filepath.Join(root, "default"), Owner: true}
	// The child cannot make its rendezvous where a file already is.
	if err := os.WriteFile(cfg.Rendezvous, nil, 0600); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	spawned, err := cfg.Start(t.Context())
	if took := time.Since(began); !spawned || !errors.Is(err, errExitedBeforeReady) || took >= startupTimeout/2 {
		t.Fatalf("spawned %v, error %v, after %v", spawned, err, took)
	}
}
