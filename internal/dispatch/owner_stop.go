package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

const stopTimeout = 3 * time.Second

// Stop asks the current instance to shut down. It reports whether a running
// dispatcher stopped and returns only after its state lock is free.
func (cfg LaunchConfig) Stop(ctx context.Context) (bool, error) {
	if !cfg.Owner {
		return false, errors.New("stopping the dispatcher requires owner ingress")
	}
	if err := CheckStateRoot(cfg.Version, cfg.StateRoot, cfg.DefaultStateRoot); err != nil {
		return false, err
	}
	wait, cancel := context.WithTimeout(ctx, stopTimeout)
	defer cancel()
	state, err := spool.OpenServerRoot(cfg.StateRoot)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer state.Close()

	poll := time.NewTicker(10 * time.Millisecond)
	defer poll.Stop()
	var wasRunning, acknowledged bool
	var instance string
	for wait.Err() == nil {
		held, err := lockState(state)
		if err != nil {
			return false, err
		}
		if !held {
			return wasRunning, nil
		}
		wasRunning = true
		if marker := cfg.readinessCandidate(); marker.Instance != "" {
			if instance != "" && marker.Instance != instance {
				return false, errors.New("another command started a replacement dispatcher; run crosshall owner stop again to stop it")
			}
			instance = marker.Instance
			if !acknowledged {
				reply, err := cfg.requestInstance(wait, wire.VerbOwnerStop, instance)
				if err == nil && unknownOwnerVerb(reply) {
					return false, errors.New("the running release does not support owner stop; find its PID with crosshall owner doctor and stop it with kill <pid>")
				}
				acknowledged = err == nil && answersForInstance(reply, instance)
			}
		}
		select {
		case <-poll.C:
		case <-wait.Done():
		}
	}
	return false, fmt.Errorf("dispatcher did not release its lock: %w", wait.Err())
}
