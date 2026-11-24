package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// Allow a dispatcher using the 250 ms fallback scan to answer a probe,
// without letting one silent instance consume the startup deadline.
const readinessProbeTimeout = 300 * time.Millisecond

// StartReady backs the explicit owner start command. Success means a
// dispatcher answered and was still the ready instance when checked.
func (cfg LaunchConfig) StartReady(ctx context.Context) error {
	if !cfg.Owner {
		return errors.New("readiness confirmation requires owner startup")
	}
	wait, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()
	deadline, _ := wait.Deadline()
	state, err := cfg.openStartState(wait)
	if err != nil {
		return err
	}
	defer state.Close()

	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var exited <-chan bool
	var launchFailed bool
	stage := "checking the dispatcher state lock"
	for wait.Err() == nil {
		held, err := lockState(state)
		if err != nil {
			return err
		}
		if wait.Err() != nil {
			break
		}
		if held {
			stage = "waiting for a valid readiness marker"
			marker := cfg.readinessCandidate()
			if marker.Instance != "" {
				stage = "waiting for a readiness probe reply"
				if cfg.probeReadiness(wait, marker) {
					stage = "confirming the responding dispatcher still owns readiness"
					held, err = lockState(state)
					if err != nil {
						return err
					}
					if held && cfg.readinessCandidate() == marker && wait.Err() == nil {
						return nil
					}
				}
			}
		} else {
			stage = "waiting for the dispatcher to hold its state lock"
			if launchFailed {
				stage = "waiting after the startup child exited unsuccessfully"
			} else if exited == nil {
				exited, err = cfg.startCandidate(state, deadline)
				if err != nil {
					return err
				}
			}
		}
		select {
		case cleanExit := <-exited:
			exited = nil
			// A contender that lost the lock exits normally. A daemon
			// that failed during startup should leave its error in the log.
			launchFailed = !cleanExit
		case <-ticker.C:
		case <-wait.Done():
		}
	}
	// The owner may have been descheduled since it last observed this stage.
	lock := "the state lock was free"
	if held, err := lockState(state); err != nil {
		lock = fmt.Sprintf("the state lock could not be checked (%v)", err)
	} else if held {
		lock = "the state lock was held"
	}
	marker := "no valid readiness marker was observed"
	if candidate := cfg.readinessCandidate(); candidate.Instance != "" {
		marker = fmt.Sprintf("a valid readiness marker named PID %d", candidate.PID)
	}
	return fmt.Errorf("dispatcher readiness was not confirmed (%w). Last observed stage was %s. "+
		"At the final check, %s and %s. See dispatcher.log.", wait.Err(), stage, lock, marker)
}

func (cfg LaunchConfig) startCandidate(state *os.Root, deadline time.Time) (<-chan bool, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	process, err := cfg.spawn(state, executable, deadline)
	if err != nil {
		return nil, err
	}
	exited := make(chan bool, 1)
	go func() {
		result, err := process.Wait()
		exited <- err == nil && result.Success()
	}()
	return exited, nil
}

func (cfg LaunchConfig) readinessCandidate() readyMarker {
	root, err := spool.OpenServerRoot(cfg.Rendezvous)
	if err != nil {
		return readyMarker{}
	}
	defer root.Close()
	marker := readReadyAt(root)
	if marker.Instance == "" || marker.PID <= 0 || marker.Version != wire.Version {
		return readyMarker{}
	}
	if marker.Lease {
		if held, err := spool.ReadyLeaseHeld(root, marker.Instance); err != nil || !held {
			return readyMarker{}
		}
	} else if marker.Probe {
		return readyMarker{}
	}
	return marker
}

func (cfg LaunchConfig) probeReadiness(ctx context.Context, marker readyMarker) bool {
	reply, err := cfg.requestInstance(ctx, wire.VerbOwnerReady, marker.Instance)
	return err == nil && (answersForInstance(reply, marker.Instance) || !marker.Probe && unknownOwnerVerb(reply))
}

func (cfg LaunchConfig) requestInstance(ctx context.Context, verb, instance string) (spool.Reply, error) {
	probe, cancel := context.WithTimeout(ctx, readinessProbeTimeout)
	defer cancel()
	args, err := json.Marshal(wire.DispatcherInstance{Instance: instance})
	if err != nil {
		return spool.Reply{}, err
	}
	client := spool.Client{StateRoot: cfg.StateRoot, Rendezvous: cfg.Rendezvous}
	return client.Owner(probe, wire.Request{Verb: verb, Args: args})
}

func answersForInstance(reply spool.Reply, instance string) bool {
	var response wire.DispatcherInstance
	return reply.Status == core.ExitOK && json.Unmarshal(reply.Data, &response) == nil && response.Instance == instance
}

func unknownOwnerVerb(reply spool.Reply) bool {
	// An older installed dispatcher answers an unknown owner verb with
	// this exact rejection.
	var refusal struct{ Message string }
	return reply.Status == core.ExitUsage && reply.Code == "" &&
		json.Unmarshal(reply.Data, &refusal) == nil && refusal.Message == "invalid command arguments"
}
