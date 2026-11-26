package spool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// The lock reports the leaf's last publish sequence, holds hooks off until
// it is released, and gives way when its context ends.
func TestALeafPublishLockHoldsHooksOff(t *testing.T) {
	leaf, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer leaf.Close()
	if err := leaf.WriteFile("hook-publish-seq", []byte("7\n"), 0600); err != nil {
		t.Fatal(err)
	}
	seq, unlock, err := LockLeafPublish(t.Context(), leaf)
	if err != nil || seq != 7 {
		t.Fatalf("seq %d, %v", seq, err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, _, err := LockLeafPublish(ctx, leaf); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a second holder: %v", err)
	}
	unlock()
	if _, again, err := LockLeafPublish(t.Context(), leaf); err != nil {
		t.Fatalf("after unlock: %v", err)
	} else {
		again()
	}
}

// A linked hook's request pins the membership it was prepared for; a stamp
// that changed before the publish lock declines it instead of relabeling
// it with the new membership's token.
func TestALinkedHookDeclinesAMembershipThatChangedBeforeTheLock(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "in"), 0700); err != nil {
		t.Fatal(err)
	}
	leaf, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer leaf.Close()
	in, err := os.OpenRoot(filepath.Join(dir, "in"))
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	if err := ident.RepublishMembership(leaf, core.Membership{BindingID: 9, Generation: 2}); err != nil {
		t.Fatal(err)
	}
	publish := func(pinned core.Membership) error {
		req := wire.Request{Verb: wire.VerbHookEvent, Attempt: wire.NewID(), Membership: &pinned, Capture: &wire.HookCapture{}}
		return publishHookRequest(t.Context(), leaf, in, req)
	}
	if err := publish(core.Membership{BindingID: 4, Generation: 1}); err == nil {
		t.Fatal("a request pinned to binding 4 was published with binding 9's token")
	}
	if err := publish(core.Membership{BindingID: 9, Generation: 2}); err != nil {
		t.Fatal(err)
	}
}
