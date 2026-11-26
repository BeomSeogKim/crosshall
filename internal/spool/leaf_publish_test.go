package spool

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
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
