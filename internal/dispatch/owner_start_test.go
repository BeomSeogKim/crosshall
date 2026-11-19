package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func TestReadinessProbeUsesOwnerIngressAndCurrentInstance(t *testing.T) {
	s := reviewServer(t)
	s.ready.Instance = "current"
	s.cfg.Handle = handlerFunc(func(context.Context, store.Tx, core.Caller, domain.Request) (core.Result, error) {
		t.Fatal("readiness probe entered the domain handler")
		return core.Result{}, nil
	})
	for _, tc := range []struct {
		name, instance string
		owner          bool
		status         core.ExitCode
		code           core.Code
	}{
		{"current", "current", true, core.ExitOK, ""},
		{"stale", "stale", true, core.ExitRefused, ""},
		{"member", "current", false, core.ExitRefused, core.CodeOwnerOnly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args, _ := json.Marshal(wire.Readiness{Instance: tc.instance})
			got := s.fixtureRequest(t, Arrival{Owner: tc.owner}, wire.Request{Verb: wire.VerbOwnerReady, Args: args})
			if got.Status != tc.status || got.Code != tc.code {
				t.Fatalf("unexpected readiness reply: %+v", got)
			}
			if tc.status == core.ExitOK {
				var out wire.Readiness
				if err := json.Unmarshal(got.Data, &out); err != nil || out.Instance != s.ready.Instance {
					t.Fatalf("readiness reply does not name the serving instance: %s, %v", got.Data, err)
				}
			}
		})
	}
}

func TestReadinessWithdrawnWhileShutdownIsInProgress(t *testing.T) {
	s, root := adapterFilesFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	stopWatching := s.withdrawReadyOnCancel(ctx)
	defer stopWatching()
	defer cancel()

	if err := s.publishReady(ctx, root); err != nil {
		t.Fatal(err)
	}
	cancel()
	deadline := time.Now().Add(time.Second)
	for {
		_, err := root.Stat("ready")
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil || time.Now().After(deadline) {
			t.Fatalf("ready marker survived cancellation: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if held, err := spool.ReadyLeaseHeld(root, s.ready.Instance); err != nil || held {
		t.Fatalf("ready lease survived cancellation: held=%t, %v", held, err)
	}
	if err := s.publishReady(ctx, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled scan could republish readiness: %v", err)
	}
}
