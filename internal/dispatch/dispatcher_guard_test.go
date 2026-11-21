//go:build testvendorhome

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
)

func TestDispatcherGuardianReapsStoppedChildBeforeReleasingWorkers(t *testing.T) {
	root := t.TempDir()
	stateDir, leaseDir := filepath.Join(root, "state"), filepath.Join(root, "lease")
	for _, dir := range []string{stateDir, leaseDir} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"runner.lock", "workers.lock"} {
		if err := os.WriteFile(filepath.Join(leaseDir, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	runner, err := os.OpenFile(filepath.Join(leaseDir, "runner.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer runner.Close()
	if err := syscall.Flock(int(runner.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	workers, err := os.OpenFile(filepath.Join(leaseDir, "workers.lock"), os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer workers.Close()
	state, err := spool.OpenServerRoot(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	t.Setenv(testDispatcherLeaseEnv, leaseDir)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cfg := LaunchConfig{StateRoot: stateDir, Rendezvous: filepath.Join(root, "rendezvous")}
	guardian, err := cfg.spawn(state, executable)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan bool, 1)
	go func() {
		result, err := guardian.Wait()
		done <- err == nil && result.Success()
	}()
	var child int
	t.Cleanup(func() {
		_ = runner.Close()
		select {
		case <-done:
		case <-time.After(4 * time.Second):
			if child > 0 {
				_ = syscall.Kill(child, syscall.SIGKILL)
			}
			select {
			case <-done:
			case <-time.After(time.Second):
				_ = guardian.Kill()
				<-done
			}
		}
	})
	deadline := time.Now().Add(4 * time.Second)
	for child == 0 {
		child = readReady(cfg.Rendezvous).PID
		if child == 0 {
			if time.Now().After(deadline) {
				t.Fatal("guarded dispatcher did not become ready")
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if child == guardian.Pid {
		t.Fatal("guardian is not supervising a separate dispatcher")
	}
	if err := syscall.Kill(child, syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(workers.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("guardian did not hold its shared workers lock: %v", err)
	}
	if err := runner.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case ok := <-done:
		done <- ok
		if !ok {
			t.Fatal("guardian did not exit cleanly on runner loss")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("guardian did not reap the stopped dispatcher")
	}
	if err := syscall.Kill(child, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("dispatcher child still exists after guardian exit: %v", err)
	}
	child = 0
	if err := syscall.Flock(int(workers.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatalf("workers lock remained held after the child was reaped: %v", err)
	}
	t.Setenv("CROSSHALL_STATE_DIR", stateDir)
	if _, err := guardTestDispatcher(leaseDir); !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("new guardian bypassed final cleanup's workers lock: %v", err)
	}
	if err := syscall.Flock(int(workers.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	if _, err := guardTestDispatcher(leaseDir); err == nil || !strings.Contains(err.Error(), "runner already ended") {
		t.Fatalf("closed runner admitted a new dispatcher: %v", err)
	}
}

func TestDispatcherGuardianRejectsUnsafeLease(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "workers.lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("workers.lock", filepath.Join(dir, "runner.lock")); err != nil {
		t.Fatal(err)
	}
	if _, err := guardTestDispatcher(dir); err == nil || !strings.Contains(err.Error(), "regular 0600 file") {
		t.Fatalf("guardian accepted a linked runner lock: %v", err)
	}
	if _, err := guardTestDispatcher("relative"); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("guardian accepted a relative lease directory: %v", err)
	}
}
