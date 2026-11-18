package spool_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func TestHookPublicationSequence(t *testing.T) {
	f := setup(t)
	f.member = ident.Identity{Vendor: "codex", Key: "ordered-hooks"}
	leaf := f.leaf(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()

	done := make(chan callResult, 1)
	go func() {
		reply, err := f.client.HookCall(ctx, f.member, "generation-a", wire.Request{Verb: wire.VerbHookEvent, OpKey: "start"}, false)
		done <- callResult{reply, err}
	}()
	var first, second struct {
		OpKey      string `json:"op_key"`
		PublishSeq uint64 `json:"publish_seq"`
	}
	attempt := claim(t, leaf, &first)
	// The first response is still pending. The next hook can publish as soon
	// as the first packet is visible, and the sequence survives the claim.
	if _, err := f.client.HookCall(ctx, f.member, "generation-a", wire.Request{Verb: wire.VerbHookEvent, OpKey: "end"}, true); err != nil {
		t.Fatal(err)
	}
	claim(t, leaf, &second)
	answer(t, leaf, attempt, attempt, core.ExitOK, "", `{}`)
	if result := <-done; result.err != nil || result.reply.Status != core.ExitOK {
		t.Fatalf("synchronous hook: %+v", result)
	}
	if first.OpKey != "start" || first.PublishSeq != 1 || second.OpKey != "end" || second.PublishSeq != 2 {
		t.Fatalf("wrong publication sequence: first %+v, second %+v", first, second)
	}
}

func TestHookPublicationLockHonorsDeadline(t *testing.T) {
	f := setup(t)
	f.member = ident.Identity{Vendor: "codex", Key: "contended-hooks"}
	leaf := f.leaf(t)
	lock, err := os.OpenFile(filepath.Join(leaf, "hook-publish.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	if _, err := f.client.HookCall(ctx, f.member, "generation-a", wire.Request{Verb: wire.VerbHookEvent, OpKey: "blocked"}, true); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("contended hook publication: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(leaf, "in"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("unexpected packet after timeout: %v (%v)", entries, err)
	}
}
