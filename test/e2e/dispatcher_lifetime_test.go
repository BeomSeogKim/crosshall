//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

const fixtureLifetimeMode = "CROSSHALL_E2E_FIXTURE_LIFETIME_MODE"
const fixtureLifetimeReport = "CROSSHALL_E2E_FIXTURE_LIFETIME_REPORT"

type fixtureProcess struct {
	PID        int
	Lock       string
	Rendezvous string
}

func TestDispatcherFollowsFixtureLifetime(t *testing.T) {
	if mode := os.Getenv(fixtureLifetimeMode); mode != "" {
		runLifetimeFixture(t, mode)
		return
	}
	for _, mode := range []string{"success", "failure", "interrupt-paused"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			report := filepath.Join(dir, "fixture.json")
			log, err := os.Create(filepath.Join(dir, "runner.log"))
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			ctx, cancel := context.WithTimeout(t.Context(), 25*time.Second)
			defer cancel()
			runner := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestDispatcherFollowsFixtureLifetime$", "-test.v")
			runner.Env = append(os.Environ(), "TMPDIR="+dir, fixtureLifetimeMode+"="+mode, fixtureLifetimeReport+"="+report)
			runner.Stdout, runner.Stderr = log, log
			input, err := runner.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := runner.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- runner.Wait() }()
			fixture := waitForFixtureProcess(t, ctx, report, log.Name())
			lock, err := os.OpenFile(fixture.Lock, os.O_RDWR, 0)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			defer func() {
				// A failing regression run must still clean up its own fixture.
				if errors.Is(syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB), syscall.EWOULDBLOCK) {
					_ = syscall.Kill(fixture.PID, syscall.SIGKILL)
					_ = lockFixtureFileUntil(lock, time.Now().Add(time.Second))
				}
				_ = os.RemoveAll(fixture.Rendezvous)
			}()
			if mode == "interrupt-paused" {
				if err := runner.Process.Signal(os.Interrupt); err != nil {
					t.Fatal(err)
				}
			} else if err := input.Close(); err != nil {
				t.Fatal(err)
			}
			if err := <-done; !expectedFixtureExit(mode, err) {
				raw, _ := os.ReadFile(log.Name())
				t.Fatalf("fixture runner %s: %v\n%s", mode, err, raw)
			}
			if err := lockFixtureFileUntil(lock, time.Now().Add(fixtureEventTimeout)); err != nil {
				t.Fatalf("dispatcher survived fixture runner %s: %v", mode, err)
			}
		})
	}
}

func runLifetimeFixture(t *testing.T, mode string) {
	w := newWorld(t)
	w.owner().must("owner", "start")
	fixture := fixtureProcess{Lock: filepath.Join(w.state, "dispatcher.lock"), Rendezvous: w.rendezvous()}
	ready, err := os.ReadFile(filepath.Join(fixture.Rendezvous, "ready"))
	if err != nil || json.Unmarshal(ready, &fixture) != nil || fixture.PID <= 0 {
		t.Fatalf("fixture dispatcher readiness: %q, %v", ready, err)
	}
	if mode == "interrupt-paused" {
		process, err := os.FindProcess(fixture.PID)
		if err != nil {
			t.Fatal(err)
		}
		pauseFixtureProcess(t, process)
	}
	raw, err := json.Marshal(fixture)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv(fixtureLifetimeReport), raw, 0600); err != nil {
		t.Fatal(err)
	}
	var release [1]byte
	_, _ = os.Stdin.Read(release[:])
	if mode == "failure" {
		t.Fatal("intentional fixture failure")
	}
}

func waitForFixtureProcess(t *testing.T, ctx context.Context, report, log string) fixtureProcess {
	t.Helper()
	for ctx.Err() == nil {
		var fixture fixtureProcess
		data, err := os.ReadFile(report)
		if err == nil && json.Unmarshal(data, &fixture) == nil && fixture.PID > 0 {
			return fixture
		}
		time.Sleep(10 * time.Millisecond)
	}
	raw, _ := os.ReadFile(log)
	t.Fatalf("fixture never reported readiness: %v\n%s", ctx.Err(), raw)
	return fixtureProcess{}
}

func expectedFixtureExit(mode string, err error) bool {
	if mode == "success" {
		return err == nil
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return false
	}
	if mode == "failure" {
		return exit.ExitCode() == 1
	}
	status, ok := exit.Sys().(syscall.WaitStatus)
	return ok && status.Signaled() && status.Signal() == syscall.SIGINT
}
