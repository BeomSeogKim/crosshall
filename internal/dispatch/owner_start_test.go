package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func TestReadinessProbeUsesOwnerIngressAndCurrentInstance(t *testing.T) {
	s := reviewServer(t)
	s.ready.Instance = "current"
	s.cfg.Handle = handlerFunc(func(context.Context, store.Tx, core.Caller, domain.Request) (core.Result, error) {
		t.Fatal("readiness probe entered the domain handler")
		return core.Result{}, nil
	})
	for _, tc := range []struct {
		name, instance string
		owner          bool
		status         core.ExitCode
		code           core.Code
	}{
		{"current", "current", true, core.ExitOK, ""},
		{"stale", "stale", true, core.ExitRefused, ""},
		{"member", "current", false, core.ExitRefused, core.CodeOwnerOnly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, _ := json.Marshal(wire.DispatcherInstance{Instance: tc.instance})
			got := s.fixtureRequest(t, Arrival{Owner: tc.owner}, wire.Request{Verb: wire.VerbOwnerReady, Args: args})
			if got.Status != tc.status || got.Code != tc.code {
				t.Fatalf("unexpected readiness reply: %+v", got)
			}
			if tc.status == core.ExitOK {
				var out wire.DispatcherInstance
				if err := json.Unmarshal(got.Data, &out); err != nil || out.Instance != s.ready.Instance {
					t.Fatalf("readiness reply does not name the serving instance: %s, %v", got.Data, err)
				}
			}
		})
	}
}

func TestOwnerStartRevalidatesReadinessAfterProbe(t *testing.T) {
	for _, scenario := range []string{"same instance", "marker replaced", "lock released", "wait expired"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				root := t.TempDir()
				cfg := LaunchConfig{Version: "test", Owner: true, StateRoot: filepath.Join(root, "state"),
					DefaultStateRoot: filepath.Join(root, "default"), Rendezvous: filepath.Join(root, "rendezvous")}
				state, err := cfg.openStartState(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				defer state.Close()
				lock, err := spool.OpenServerFile(state, "dispatcher.lock", os.O_CREATE|os.O_RDWR)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Close()
				if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
					t.Fatal(err)
				}
				rendezvous, err := openRendezvous(cfg.Rendezvous)
				if err != nil {
					t.Fatal(err)
				}
				defer rendezvous.Close()
				publishMarker := func(instance string) {
					lease, err := spool.HoldReadyLease(rendezvous, instance)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() { lease.Close() })
					if err := spool.PublishServerFile(rendezvous, "ready", newReadyMarker(instance)); err != nil {
						t.Fatal(err)
					}
				}
				const instance = "original"
				publishMarker(instance)
				owner, err := spool.OpenServerChild(state, "owner", false)
				if err != nil {
					t.Fatal(err)
				}
				defer owner.Close()

				// Expire before the owner's next attempt and between two 1 ms spool polls.
				deadline := time.Now().Add(5500 * time.Microsecond)
				ctx, cancel := context.WithDeadline(t.Context(), deadline)
				defer cancel()
				done := make(chan error, 1)
				go func() { done <- cfg.StartReady(ctx) }()
				synctest.Wait()

				var attempt string
				err = spool.DrainServerSpool(owner, func(id string, raw []byte) (core.Result, error) {
					var req wire.Request
					var asked wire.DispatcherInstance
					if err := json.Unmarshal(raw, &req); err != nil || req.Verb != wire.VerbOwnerReady ||
						json.Unmarshal(req.Args, &asked) != nil || asked.Instance != instance {
						t.Fatalf("unexpected probe %q (%v)", raw, err)
					}
					attempt = id
					switch scenario {
					case "marker replaced":
						publishMarker("replacement")
					case "lock released":
						if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
							t.Fatal(err)
						}
					case "wait expired":
						// The claimed probe checks for its reply again on cancellation.
						time.Sleep(time.Until(deadline) - time.Nanosecond)
					}
					return core.Result{Status: core.ExitOK, Data: json.RawMessage(`{"instance":"original"}`)}, nil
				})
				if err != nil || attempt == "" {
					t.Fatalf("serve the readiness probe: attempt %q, error %v", attempt, err)
				}
				got := <-done
				if _, err := owner.Stat(filepath.Join("out", attempt+".json")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("owner did not read the probe response: %v", err)
				}
				if scenario == "same instance" {
					if got != nil {
						t.Fatalf("owner rejected the current live instance: %v", got)
					}
				} else if !errors.Is(got, context.DeadlineExceeded) {
					t.Fatalf("owner start error %v, want deadline exceeded", got)
				}
			})
		})
	}
}

func TestReadinessWithdrawnWhileShutdownIsInProgress(t *testing.T) {
	s, root := adapterFilesFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	stopWatching := s.withdrawReadyOnCancel(ctx)
	defer stopWatching()
	defer cancel()

	if err := s.publishReady(ctx, root); err != nil {
		t.Fatal(err)
	}
	cancel()
	deadline := time.Now().Add(time.Second)
	for {
		_, err := root.Stat("ready")
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil || time.Now().After(deadline) {
			t.Fatalf("ready marker survived cancellation: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if held, err := spool.ReadyLeaseHeld(root, s.ready.Instance); err != nil || held {
		t.Fatalf("ready lease survived cancellation: held=%t, %v", held, err)
	}
	if err := s.publishReady(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled scan could republish readiness: %v", err)
	}
}

func TestOwnerDeadlineAlsoExpiresItsStartupChild(t *testing.T) {
	root := t.TempDir()
	cfg := LaunchConfig{Version: "test", Owner: true, StateRoot: filepath.Join(root, "state"),
		DefaultStateRoot: filepath.Join(root, "default"), Rendezvous: filepath.Join(root, "rendezvous")}
	state, err := cfg.openStartState(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	lock, err := spool.OpenServerFile(state, "dispatcher.lock", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := cfg.StartReady(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("owner startup under shared lock: %v", err)
	}
	// A fresh three-second timeout in the child would let it start now.
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool {
		if marker := readReady(cfg.Rendezvous); marker.PID > 0 {
			_ = syscall.Kill(marker.PID, syscall.SIGTERM)
			t.Fatalf("dispatcher %d became ready after its owner timed out", marker.PID)
		}
		log, err := state.ReadFile("dispatcher.log")
		return err == nil && strings.Contains(string(log), context.DeadlineExceeded.Error())
	})
	if _, err := state.Stat("store.sqlite3"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dispatcher opened the store after its owner timed out: stat error %v", err)
	}
}
