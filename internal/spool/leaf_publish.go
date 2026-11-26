package spool

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

// LockLeafPublish takes a leaf's hook publish lock, waiting until ctx
// ends, and returns the leaf's last publish sequence (#400, design 6a). A
// change applied before unlock precedes every hook fact published with a
// higher sequence. The caller calls unlock once its change is durable.
func LockLeafPublish(ctx context.Context, leaf *os.Root) (seq uint64, unlock func(), err error) {
	lock, err := OpenServerFile(leaf, "hook-publish.lock", os.O_CREATE|os.O_RDWR)
	if err != nil {
		return 0, nil, err
	}
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			lock.Close()
			return 0, nil, err
		}
		select {
		case <-tick.C:
		case <-ctx.Done():
			lock.Close()
			return 0, nil, ctx.Err()
		}
	}
	seq, err = readSequence(leaf, "hook-publish-seq")
	if err != nil {
		lock.Close() // closing the descriptor releases the lock
		return 0, nil, err
	}
	return seq, func() { lock.Close() }, nil
}
