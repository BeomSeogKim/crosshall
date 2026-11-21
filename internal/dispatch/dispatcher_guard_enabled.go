//go:build testvendorhome

package dispatch

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/spool"
)

const (
	testDispatcherLeaseEnv = "CROSSHALL_TEST_DISPATCHER_LEASE_DIR"
	testDispatcherGuardian = "__test_dispatch_guardian"
	testDispatcherPoll     = 25 * time.Millisecond
)

func dispatcherSubcommand() string {
	if _, ok := os.LookupEnv(testDispatcherLeaseEnv); ok {
		return testDispatcherGuardian
	}
	return "__dispatch"
}

func appendTestDispatcherEnv(env []string) []string {
	if dir, ok := os.LookupEnv(testDispatcherLeaseEnv); ok {
		return append(env, testDispatcherLeaseEnv+"="+dir)
	}
	return env
}

func RunTestDispatcherGuardian(args []string) (bool, int) {
	if len(args) != 1 || args[0] != testDispatcherGuardian {
		return false, 0
	}
	code, err := guardTestDispatcher(os.Getenv(testDispatcherLeaseEnv))
	if err != nil {
		fmt.Fprintln(os.Stderr, "test dispatcher guardian:", err)
		return true, 1
	}
	return true, code
}

func guardTestDispatcher(path string) (int, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return 1, errors.New("expected an absolute, clean dispatcher lease directory")
	}
	lease, err := spool.OpenServerRoot(path)
	if err != nil {
		return 1, err
	}
	defer lease.Close()
	workers, err := spool.OpenServerFile(lease, "workers.lock", os.O_RDWR)
	if err != nil {
		return 1, err
	}
	defer workers.Close()
	if err := syscall.Flock(int(workers.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		return 1, fmt.Errorf("join dispatcher fixture workers: %w", err)
	}
	runner, err := spool.OpenServerFile(lease, "runner.lock", os.O_RDWR)
	if err != nil {
		return 1, err
	}
	defer runner.Close()
	active, err := testDispatcherRunnerActive(runner)
	if err != nil {
		return 1, err
	}
	if !active {
		return 1, errors.New("dispatcher fixture runner already ended")
	}

	executable, err := os.Executable()
	if err != nil {
		return 1, err
	}
	cmd := exec.Command(executable, "__dispatch")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return 1, err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ticker := time.NewTicker(testDispatcherPoll)
	defer ticker.Stop()
	for {
		select {
		case err := <-done:
			var exited *exec.ExitError
			switch {
			case err == nil:
				return 0, nil
			case errors.As(err, &exited):
				return max(exited.ExitCode(), 1), nil
			default:
				return 1, err
			}
		case <-ticker.C:
			if active, err = testDispatcherRunnerActive(runner); active && err == nil {
				continue
			}
			if killErr := cmd.Process.Kill(); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
				fmt.Fprintln(os.Stderr, "test dispatcher guardian: stop child:", killErr)
			}
			// Keep the shared workers lock until Wait has reaped the child.
			<-done
			return 0, err
		}
	}
}

func testDispatcherRunnerActive(runner *os.File) (bool, error) {
	fd := int(runner.Fd())
	err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case errors.Is(err, syscall.EWOULDBLOCK):
		return true, nil
	case err != nil:
		return false, fmt.Errorf("check dispatcher fixture runner: %w", err)
	}
	return false, syscall.Flock(fd, syscall.LOCK_UN)
}
