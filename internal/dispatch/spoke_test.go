package dispatch

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func TestSpokeOwnerControlsRetireOnlyTheMatchingInstance(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rendezvous := t.TempDir()
	if err := os.Chmod(rendezvous, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := openRendezvousRoot(rendezvous)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	ready := newReadyMarker("spoke-instance")
	ready.Spoke, ready.Hello = "mini", nil
	s := server{cfg: Config{LaunchConfig: LaunchConfig{Rendezvous: rendezvous}}, ready: ready, cancel: cancel}
	defer s.withdrawReady()
	if err := s.publishReady(ctx, root); err != nil {
		t.Fatal(err)
	}
	call := func(verb, instance, digest string) core.Result {
		t.Helper()
		args, err := json.Marshal(wire.DispatcherInstance{Instance: instance})
		if err != nil {
			t.Fatal(err)
		}
		req := wire.Request{Version: wire.Version, Attempt: "attempt", Verb: verb, Args: args,
			ExpiresAtMS: time.Now().Add(time.Minute).UnixMilli(), KeyDigest: digest}
		packet, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		result, err := s.spokeOwnerCommand(ctx, req.Attempt, packet)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	if got := call(wire.VerbOwnerStop, "previous-instance", ""); got.Status != core.ExitRefused || ctx.Err() != nil {
		t.Fatalf("stale stop = %+v, context = %v", got, ctx.Err())
	}
	if got := call(wire.VerbOwnerStop, ready.Instance, "member"); got.Code != core.CodeOwnerOnly || ctx.Err() != nil {
		t.Fatalf("member stop = %+v, context = %v", got, ctx.Err())
	}
	if got := call("owner channel create", ready.Instance, ""); got.Code != core.CodeDispatcherUnavailable {
		t.Fatalf("local command = %+v", got)
	}
	if got := call(wire.VerbOwnerReady, ready.Instance, ""); !answersForInstance(spool.Reply{Response: wire.Response{Result: got}}, ready.Instance) {
		t.Fatalf("probe response = %+v", got)
	}
	if got := call(wire.VerbOwnerStop, ready.Instance, ""); got.Status != core.ExitOK || ctx.Err() == nil {
		t.Fatalf("current stop = %+v, context = %v", got, ctx.Err())
	}
	if readReadyAt(root).Instance != "" {
		t.Fatal("stop left a ready marker")
	}
	if held, err := spool.ReadyLeaseHeld(root, ready.Instance); err != nil || held {
		t.Fatalf("lease after stop = %v, %v", held, err)
	}
}
