package spool

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/wire"
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

func (c Client) DispatcherRunning() bool {
	root, err := ident.OpenPrivateDir(c.Rendezvous)
	if err != nil {
		return false
	}
	defer root.Close()
	raw, err := ident.ReadPrivateFile(root, "ready", 4096)
	if err != nil {
		return false
	}
	var marker struct {
		Instance string `json:"instance"`
		Version  int    `json:"v"`
		PID      int    `json:"pid"`
		Lease    bool   `json:"lease"`
	}
	if json.Unmarshal(raw, &marker) != nil || marker.Version != wire.Version ||
		marker.Instance == "" || marker.PID <= 0 {
		return false
	}
	if marker.Lease {
		held, err := ReadyLeaseHeld(root, marker.Instance)
		if held {
			return true
		}
		if !errors.Is(err, os.ErrPermission) {
			return false
		}
	}
	// Older dispatchers omit the lease, and restricted clients can be denied flock.
	err = syscall.Kill(marker.PID, 0)
	return err == nil || errors.Is(err, os.ErrPermission)
}

// memberRendezvous returns where member spools are: the rendezvous
// itself, or the member rendezvous a linked device's dispatcher names in
// its ready marker (#400).
func memberRendezvous(rendezvous string) string {
	root, err := ident.OpenPrivateDir(rendezvous)
	if err != nil {
		return rendezvous
	}
	defer root.Close()
	raw, err := ident.ReadPrivateFile(root, "ready", 4096)
	if err != nil {
		return rendezvous
	}
	var marker struct {
		Members string `json:"members"`
	}
	if json.Unmarshal(raw, &marker) != nil || marker.Members == "" || !filepath.IsAbs(marker.Members) {
		return rendezvous
	}
	return marker.Members
}
