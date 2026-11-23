//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

const silentInstance = "silent-fixture"

func holdSilentDispatcher(t *testing.T, w *world, publishReady bool) func() {
	t.Helper()
	for _, dir := range []string{w.state, w.rendezvous()} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	lock, err := os.OpenFile(filepath.Join(w.state, "dispatcher.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Close() })
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if !publishReady {
		return func() { lock.Close() }
	}
	root, err := spool.OpenServerRoot(w.rendezvous())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	lease, err := spool.HoldReadyLease(root, silentInstance)
	if err != nil {
		t.Fatal(err)
	}
	release := func() {
		lease.Close()
		lock.Close()
	}
	t.Cleanup(release)
	if err := spool.PublishServerFile(root, "ready", map[string]any{
		"v": wire.Version, "pid": os.Getpid(), "instance": silentInstance, "lease": true, "probe": true,
	}); err != nil {
		t.Fatal(err)
	}
	return release
}

func TestOwnerStartTimesOutWhenIncumbentIsSilent(t *testing.T) {
	for _, tc := range []struct {
		name, stage, marker string
		publishReady        bool
	}{
		{"no marker", "waiting for a valid readiness marker", "no valid readiness marker was observed", false},
		{"silent probe", "waiting for a readiness probe reply", fmt.Sprintf("a valid readiness marker named PID %d", os.Getpid()), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			holdSilentDispatcher(t, w, tc.publishReady)
			got := w.owner().run("owner", "start")
			if got.exit != 1 {
				t.Fatalf("owner start exit %d, want 1: %s", got.exit, got.stdout)
			}
			assertOwnerStartTimeout(t, got.stdout, tc.stage, "the state lock was held and "+tc.marker)
		})
	}
}

func ownerStartAtReadinessProbe(t *testing.T, w *world) (*os.Process, *bytes.Buffer, <-chan error) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), binary, "--json", "owner", "start")
	cmd.Dir, cmd.Env = w.home, w.owner().env()
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timeout := time.NewTimer(3 * time.Second)
	defer timeout.Stop()
	poll := time.NewTicker(5 * time.Millisecond)
	defer poll.Stop()

	// Keep the incumbent locked until the launcher tries to contact it.
	in := filepath.Join(w.state, "owner", "in")
	for {
		select {
		case err := <-done:
			t.Fatalf("owner start returned with the silent incumbent still locked: %v, %s", err, output.String())
		case <-timeout.C:
			t.Fatal("owner start sent no readiness probe")
		case <-poll.C:
			files, _ := os.ReadDir(in)
			for _, file := range files {
				if !strings.HasSuffix(file.Name(), ".json") {
					continue
				}
				raw, err := os.ReadFile(filepath.Join(in, file.Name()))
				if os.IsNotExist(err) {
					continue // this probe expired before the read
				}
				if err != nil {
					t.Fatal(err)
				}
				var req wire.Request
				if err := json.Unmarshal(raw, &req); err != nil || req.Verb != wire.VerbOwnerReady {
					t.Fatalf("expected an owner readiness probe, got verb %q (%v)", req.Verb, err)
				}
				return cmd.Process, &output, done
			}
		}
	}
}

func TestOwnerStartSeparatesLastObservedStageFromFinalState(t *testing.T) {
	w := newWorld(t)
	release := holdSilentDispatcher(t, w, true)
	owner, output, done := ownerStartAtReadinessProbe(t, w)
	pauseFixtureProcess(t, owner)
	// The probe proves the three-second owner startup deadline has started.
	// Keep its caller stopped through expiry; the incumbent goes away meanwhile.
	time.Sleep(3 * time.Second)
	if err := os.Remove(filepath.Join(w.rendezvous(), "ready")); err != nil {
		t.Fatal(err)
	}
	release()
	resumeFixtureProcess(t, owner)

	select {
	case err := <-done:
		exit, ok := err.(*exec.ExitError)
		if !ok || exit.ExitCode() != 1 {
			t.Fatalf("owner start exit: %v, %s", err, output)
		}
		var result struct{ Message string }
		if err := json.Unmarshal(output.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		assertOwnerStartTimeout(t, result.Message, "waiting for a readiness probe reply",
			"the state lock was free and no valid readiness marker was observed")
	case <-time.After(fixtureEventTimeout):
		t.Fatal("owner start did not return after its deadline")
	}
}

func assertOwnerStartTimeout(t *testing.T, message, stage, final string) {
	t.Helper()
	for _, want := range []string{
		"dispatcher readiness was not confirmed",
		"context deadline exceeded",
		"Last observed stage was " + stage + ".",
		"At the final check, " + final + ".",
	} {
		if !strings.Contains(message, want) {
			t.Fatalf("owner start error %q does not contain %q", message, want)
		}
	}
}

func TestOwnerStartWaitsForSilentIncumbentToReleaseLock(t *testing.T) {
	w := newWorld(t)
	release := holdSilentDispatcher(t, w, true)
	_, output, done := ownerStartAtReadinessProbe(t, w)
	release()
	select {
	case err := <-done:
		var response struct{ OK bool }
		if err != nil || json.Unmarshal(output.Bytes(), &response) != nil || !response.OK {
			t.Fatalf("replacement did not become ready: %v, %s", err, output.String())
		}
	case <-time.After(4 * time.Second):
		t.Fatal("owner start did not return after the incumbent released its lock")
	}

	raw, err := os.ReadFile(filepath.Join(w.rendezvous(), "ready"))
	var marker struct{ Instance string }
	if err != nil || json.Unmarshal(raw, &marker) != nil || marker.Instance == "" || marker.Instance == silentInstance {
		t.Fatalf("replacement did not publish a new ready instance: %s, %v", raw, err)
	}
	assertReadyLease(t, w.rendezvous(), marker.Instance, true)
}

func TestDispatcherStartupDistinguishesSharedObserversFromAnIncumbent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		lock    int
		release bool
	}{
		{"shared observer releases", syscall.LOCK_SH, true},
		{"shared observer remains", syscall.LOCK_SH, false},
		{"exclusive incumbent remains", syscall.LOCK_EX, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newWorld(t)
			if err := os.Mkdir(w.state, 0700); err != nil {
				t.Fatal(err)
			}
			lock, err := os.OpenFile(filepath.Join(w.state, "dispatcher.lock"), os.O_CREATE|os.O_RDWR, 0600)
			if err != nil {
				t.Fatal(err)
			}
			defer lock.Close()
			if err := syscall.Flock(int(lock.Fd()), tc.lock|syscall.LOCK_NB); err != nil {
				t.Fatal(err)
			}

			// One guardian launches exactly one candidate; a replacement
			// cannot hide that candidate's loss of a shared-lock race.
			cmd := exec.Command(binary, "__test_dispatch_guardian")
			cmd.Dir, cmd.Env = w.home, w.owner().env()
			var output bytes.Buffer
			cmd.Stdout, cmd.Stderr = &output, &output
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			deadline := time.After(fixtureEventTimeout)
			ready := filepath.Join(w.rendezvous(), "ready")
			if !tc.release {
				select {
				case err := <-done:
					if tc.lock == syscall.LOCK_EX && err != nil {
						t.Fatalf("candidate did not defer to the incumbent: %v, %s", err, &output)
					}
					if tc.lock == syscall.LOCK_SH && (err == nil || !strings.Contains(output.String(), "acquiring dispatcher lock: context deadline exceeded")) {
						t.Fatalf("expected a shared lock timeout, got %v, %q", err, &output)
					}
				case <-deadline:
					t.Fatal("candidate never returned under a persistent lock")
				}
				if _, err := os.Stat(ready); !os.IsNotExist(err) {
					t.Fatalf("readiness marker while lock was held: %v", err)
				}
				if _, err := os.Stat(filepath.Join(w.state, "store.sqlite3")); !os.IsNotExist(err) {
					t.Fatalf("candidate opened the store while lock was held: %v", err)
				}
				return
			}

			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
			var candidate int
			for {
				select {
				case err := <-done:
					t.Fatalf("candidate exited before reaching readiness: %v, %s", err, &output)
				case <-deadline:
					t.Fatalf("candidate %d never reached readiness", candidate)
				case <-tick.C:
					if candidate == 0 {
						for _, pid := range w.dispatcherPIDs() {
							if pid != os.Getpid() {
								candidate = pid
								break
							}
						}
						if candidate != 0 {
							if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
								t.Fatal(err)
							}
						}
						continue
					}
					raw, err := os.ReadFile(ready)
					if os.IsNotExist(err) {
						continue
					}
					var marker struct{ PID int }
					if err != nil || json.Unmarshal(raw, &marker) != nil || marker.PID != candidate {
						t.Fatalf("candidate %d did not own readiness: %s (%v)", candidate, raw, err)
					}
					w.owner().must("owner", "start")
					return
				}
			}
		})
	}
}
