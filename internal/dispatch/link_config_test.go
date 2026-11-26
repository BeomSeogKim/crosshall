package dispatch

import (
	"errors"
	"os"
	"syscall"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/spool"
)

// A dispatcher starting while a link change runs leaves the enrollment
// record alone: the change may be writing a new one at the same path, and
// removing it there would cost the device a credential rotation (#418).
func TestStartupLeavesTheEnrollmentRecordToARunningLinkChange(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	state, err := spool.OpenServerRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	profile := linkConfig{linkEnrollment: linkEnrollment{ID: "done"}, Device: "dev-1"}
	if err := spool.PublishServerFile(state, spokeEnrollFile, profile.linkEnrollment); err != nil {
		t.Fatal(err)
	}
	present := func() bool {
		_, err := state.Lstat(spokeEnrollFile)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		return err == nil
	}

	change, err := spool.OpenServerFile(state, "link.lock", os.O_CREATE|os.O_RDWR)
	if err != nil {
		t.Fatal(err)
	}
	defer change.Close()
	if err := syscall.Flock(int(change.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	consumeEnrollment(state, profile)
	if !present() {
		t.Fatal("startup removed the enrollment record while a link change held the lock")
	}

	if err := syscall.Flock(int(change.Fd()), syscall.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	consumeEnrollment(state, profile)
	if present() {
		t.Fatal("startup kept a finished enrollment record with no link change running")
	}
}
