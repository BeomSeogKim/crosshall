package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/hook"
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
			// A crash leaves the policy behind for the hooks; an owner
			// stop withdraws it in any case. Hold the lock meanwhile so a
			// dispatcher starting now cannot publish and then lose it.
			withdrawn, err := withdrawStoppedPolicy(state, cfg.Rendezvous)
			if err != nil {
				return false, err
			}
			if withdrawn {
				return wasRunning, nil
			}
			// Another process holds the lock now: look again.
		} else {
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
		}
		select {
		case <-poll.C:
		case <-wait.Done():
		}
	}
	return false, fmt.Errorf("dispatcher did not release its lock: %w", wait.Err())
}

// withdrawStoppedPolicy removes both policy copies under the state lock. It
// reports false when a dispatcher holds the lock.
func withdrawStoppedPolicy(state *os.Root, rendezvous string) (bool, error) {
	lock, err := spool.OpenServerFile(state, "dispatcher.lock", os.O_RDWR)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil // never started here: no policy was published
	}
	if err != nil {
		return false, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return false, nil
		}
		return false, err
	}
	if err := state.Remove(hook.StatePolicyName); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	root, err := spool.OpenServerRoot(rendezvous)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	defer root.Close()
	if err := root.Remove(adapterPolicyName); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	return true, nil
}
