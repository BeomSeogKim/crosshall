package dispatch

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func TestHookRequestUsesLeafVendorAndCurrentPolicy(t *testing.T) {
	s := reviewServer(t)
	s.ready.Instance = "dispatcher-instance"
	s.fixtureSQL(t, `INSERT INTO adapter_policy(adapter,enabled,reminder,epoch,changed_at_ms)
		VALUES ('codex',1,0,3,1)`)
	resolved := Resolution{Caller: core.Caller{Role: core.RoleMember}, Vendor: "codex"}
	s.cfg.Resolve = func(context.Context, store.Tx, Arrival, wire.Request) (Resolution, error) {
		return resolved, nil
	}
	var calls int
	s.cfg.Decode = func(req wire.Request, _ Resolution) (domain.Request, error) {
		return domain.Request{OpKey: req.OpKey, Ack: req.Ack, Command: domain.Who{}}, nil
	}
	s.cfg.Handle = handlerFunc(func(_ context.Context, _ store.Tx, _ core.Caller, req domain.Request) (core.Result, error) {
		calls++
		if req.Adapter == nil || req.Adapter.Instance != s.ready.Instance ||
			req.Adapter.Generation != "verified-generation" || req.Ack != 0 {
			t.Fatalf("hook reached the domain with wrong context: %+v (ack %d)", req.Adapter, req.Ack)
		}
		return domain.OK(wire.HookResult{})
	})
	noReply := false
	args := wire.HookEventArgs{
		Profile: wire.HookProfile{Adapter: "codex"}, LaunchRef: "session", EventRef: "prompt-1",
		Fact: wire.HookPromptSubmitted, Epoch: 3,
	}
	req := wire.Request{
		Verb: wire.VerbHookEvent, OpKey: "hook:codex:session:prompt-1",
		Generation: "verified-generation", Reply: &noReply, Ack: 42,
	}
	send := func(arrival Arrival, code core.ExitCode) {
		t.Helper()
		data, err := json.Marshal(args)
		if err != nil {
			t.Fatal(err)
		}
		req.Args = data
		if got := s.fixtureRequest(t, arrival, req); got.Status != code {
			t.Fatalf("wanted exit %d, got %+v", code, got)
		}
	}

	send(Arrival{}, core.ExitOK)
	send(Arrival{Owner: true}, core.ExitUsage)
	args.WantOutput = true
	send(Arrival{}, core.ExitUsage)
	args.WantOutput = false
	args.Epoch = 2
	send(Arrival{}, core.ExitRefused)
	args.Epoch = 3
	resolved.Vendor = "claude"
	send(Arrival{}, core.ExitRefused)
	resolved.Vendor = "codex"
	resolved.Caller.Role = core.RoleUnenrolled
	send(Arrival{}, core.ExitNotEnrolled)
	resolved.Caller.Role = core.RoleMember
	s.fixtureSQL(t, `UPDATE adapter_policy SET enabled=0, epoch=4 WHERE adapter='codex'`)
	args.Epoch = 4
	send(Arrival{}, core.ExitRefused)
	if calls != 1 {
		t.Fatalf("%d requests reached the domain, want one", calls)
	}
}
