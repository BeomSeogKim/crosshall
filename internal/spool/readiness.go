package spool

import (
	"errors"
	"io"
	"os"
	"syscall"
)

const ReadyLeaseFile = "ready.lease"

// HoldReadyLease publishes a private instance lease and returns its locked file.
// Close the file to release the lease.
func HoldReadyLease(root *os.Root, instance string) (*os.File, error) {
	const staged = ReadyLeaseFile + ".part"
	if err := root.Remove(staged); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	file, err := root.OpenFile(staged, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	defer root.Remove(staged)
	if err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
		_, err = file.WriteString(instance)
	}
	if err == nil {
		err = root.Rename(staged, ReadyLeaseFile)
	}
	if err != nil {
		file.Close()
		return nil, err
	}
	return file, nil
}

// ReadyLeaseHeld checks the named instance without waiting or writing.
func ReadyLeaseHeld(root *os.Root, instance string) (bool, error) {
	if instance == "" {
		return false, nil
	}
	file, err := OpenServerFile(root, ReadyLeaseFile, os.O_RDONLY)
	if err != nil {
		return false, err
	}
	defer file.Close()
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if !errors.Is(err, syscall.EWOULDBLOCK) {
		return false, err
	}
	data, err := io.ReadAll(io.LimitReader(file, int64(len(instance))+1))
	return err == nil && string(data) == instance, err
}
