package hook

import (
	"errors"
	"os"
	"syscall"

	"github.com/BeomSeogKim/crosshall/internal/spool"
)

const joinHintLock = "join-hint.lock"

// RetireJoinHintFiles cleans a fenced leaf while leaving its refusal spool usable.
// A hook already writing may still hold the lock. The next scan can retry.
func RetireJoinHintFiles(leaf *os.Root) error {
	if err := removeJoinHintFile(leaf, JoinHintOfferFile); err != nil {
		return err
	}
	lock, err := spool.OpenServerFile(leaf, joinHintLock, os.O_RDWR)
	if errors.Is(err, os.ErrNotExist) {
		return removeJoinHintFile(leaf, seenJoinHint)
	}
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); errors.Is(err, syscall.EWOULDBLOCK) {
		return nil
	} else if err != nil {
		return err
	}
	if err := removeJoinHintFile(leaf, seenJoinHint); err != nil {
		return err
	}
	return removeJoinHintFile(leaf, joinHintLock)
}

func removeJoinHintFile(leaf *os.Root, name string) error {
	err := leaf.Remove(name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
