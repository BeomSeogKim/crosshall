package spool_test

import (
	"context"
	"io/fs"
	"os"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func TestRosterReceiptsStayWithTheMembershipThatRenderedThem(t *testing.T) {
	f := setup(t)
	path := f.leaf(t)
	leaf, err := ident.OpenPrivateDir(path)
	if err != nil {
		t.Fatal(err)
	}
	defer leaf.Close()
	A := wire.Membership{BindingID: 11, Generation: 1}
	B := wire.Membership{BindingID: 22, Generation: 2}
	if err := ident.RepublishMembership(leaf, A); err != nil {
		t.Fatal(err)
	}
	done := callMember(t.Context(), f, wire.Request{Verb: "who"})
	var req wire.Request
	attempt := claim(t, path, &req)
	if req.Membership == nil || *req.Membership != A {
		t.Fatalf("first command captured %+v, want %+v", req.Membership, A)
	}
	answer(t, path, attempt, attempt, core.ExitOK, "", `{"roster_seq":40,"rules_given":true}`)
	old := <-done
	if old.err != nil || old.reply.Status != core.ExitOK {
		t.Fatalf("first reply: %+v, %v", old.reply, old.err)
	}
	if err := ident.RepublishMembership(leaf, B); err != nil {
		t.Fatal(err)
	}
	if err := old.reply.Acknowledge(); err != nil {
		t.Fatal(err)
	}
	if _, err := leaf.Stat("ack"); !os.IsNotExist(err) {
		t.Fatalf("late A renderer wrote a roster receipt: %v", err)
	}
	if err := ident.PublishRosterReceipt(leaf, "ack", A, 40); err != nil {
		t.Fatal(err)
	}
	done = callMember(t.Context(), f, wire.Request{Verb: "who"})
	attempt = claim(t, path, &req)
	if req.Membership == nil || *req.Membership != B || req.Ack != 0 {
		t.Fatalf("B inherited an old roster cursor: %+v", req)
	}
	answer(t, path, attempt, attempt, core.ExitOK, "", `{"roster_seq":2}`)
	current := <-done
	if current.err != nil || current.reply.Status != core.ExitOK {
		t.Fatalf("B reply: %+v, %v", current.reply, current.err)
	}
	if err := current.reply.Acknowledge(); err != nil {
		t.Fatal(err)
	}
	if cursor, err := ident.ReadRosterReceipt(leaf, "ack", B); err != nil || cursor != 2 {
		t.Fatalf("B cursor=%d, err=%v", cursor, err)
	}
	if cursor, err := ident.ReadRosterReceipt(leaf, "ack", wire.Membership{BindingID: 11, Generation: 3}); err != nil || cursor != 0 {
		t.Fatalf("reactivated A inherited cursor=%d, err=%v", cursor, err)
	}
}

func TestHookCallRejectsAnEarlierMembershipBeforePublishing(t *testing.T) {
	f := setup(t)
	f.member = ident.Identity{Vendor: "codex", Key: "fixture-thread"}
	path := f.leaf(t)
	leaf, err := ident.OpenPrivateDir(path)
	if err != nil {
		t.Fatal(err)
	}
	defer leaf.Close()
	if err := ident.RepublishMembership(leaf, wire.Membership{BindingID: 11, Generation: 3}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	_, err = f.client.HookCall(ctx, f.member, "generation-a", wire.Request{
		Verb: wire.VerbHookRoute, OpKey: "route",
		Membership: &wire.Membership{BindingID: 11, Generation: 1},
	}, true)
	if err == nil {
		t.Fatal("the first visit's route callback reached the reactivated binding")
	}
	files, err := fs.ReadDir(leaf.FS(), "in")
	if err != nil || len(files) != 0 {
		t.Fatalf("a stale hook was published: %v (%v)", files, err)
	}
}

func TestWaitCanPinTheMissingMembershipMarkerDuringUpgrade(t *testing.T) {
	f := setup(t)
	path := f.leaf(t)
	leaf, err := ident.OpenPrivateDir(path)
	if err != nil {
		t.Fatal(err)
	}
	defer leaf.Close()
	done := callMember(t.Context(), f, wire.Request{Verb: "wait"})
	var request wire.Request
	attempt := claim(t, path, &request)
	if request.Membership != nil {
		t.Fatalf("old leaf sent an explicit membership %+v", request.Membership)
	}
	answer(t, path, attempt, attempt, core.ExitOK, "", `{}`)
	first := <-done
	if first.err != nil || !first.reply.MembershipCaptured || first.reply.RequestMembership != nil {
		t.Fatalf("first poll did not capture the missing marker: %+v, %v", first.reply, first.err)
	}
	if err := ident.RepublishMembership(leaf, wire.Membership{BindingID: 22, Generation: 2}); err != nil {
		t.Fatal(err)
	}
	second := make(chan callResult, 1)
	go func() {
		reply, err := f.client.MemberPinned(t.Context(), f.member, wire.Request{Verb: "wait", Membership: first.reply.RequestMembership})
		second <- callResult{reply, err}
	}()
	attempt = claim(t, path, &request)
	if request.Membership != nil {
		t.Fatalf("subsequent wait poll acquired the new membership %+v", request.Membership)
	}
	answer(t, path, attempt, attempt, core.ExitRefused, core.CodeBindingChanged, `{}`)
	if reply := <-second; reply.err != nil || reply.reply.Code != core.CodeBindingChanged {
		t.Fatalf("second poll: %+v, %v", reply.reply, reply.err)
	}
}
