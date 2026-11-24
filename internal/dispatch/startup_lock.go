package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// The launcher and child share the host's monotonic clock. A delayed child
// gets only the time left for its owner to confirm startup.
const lockCutoffEnv = "CROSSHALL_INTERNAL_DISPATCH_LOCK_CUTOFF_NS"

func monotonicNanos() (int64, error) {
	var ts unix.Timespec
	err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts)
	return ts.Nano(), err
}

func acquireDispatcherLock(ctx context.Context, lock *os.File) (bool, error) {
	now, err := monotonicNanos()
	if err != nil {
		return false, err
	}
	cutoff := now + startupTimeout.Nanoseconds()
	if raw, ok := os.LookupEnv(lockCutoffEnv); ok {
		ownerCutoff, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || ownerCutoff <= 0 {
			return false, errors.New("invalid dispatcher lock cutoff")
		}
		cutoff = min(cutoff, ownerCutoff)
	}
	fd := int(lock.Fd())
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		remaining, err := lockTimeLeft(cutoff)
		if err != nil {
			return false, err
		}
		err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			// A suspended candidate can wake holding the lock after expiry.
			// Serve closes the descriptor on error before it opens the store.
			_, err := lockTimeLeft(cutoff)
			return err == nil, err
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return false, err
		}
		// A shared observer also blocks an exclusive flock. Only yield
		// the startup to an incumbent if it blocks the shared probe too.
		if err := syscall.Flock(fd, syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return false, nil
			}
			return false, err
		}
		if err := syscall.Flock(fd, syscall.LOCK_UN); err != nil {
			return false, err
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(min(10*time.Millisecond, remaining)):
		}
	}
}

func lockTimeLeft(cutoff int64) (time.Duration, error) {
	now, err := monotonicNanos()
	if err != nil {
		return 0, err
	}
	if now >= cutoff {
		return 0, fmt.Errorf("acquiring dispatcher lock: %w", context.DeadlineExceeded)
	}
	return time.Duration(cutoff - now), nil
}
