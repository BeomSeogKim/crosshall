package main

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/cli"
	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
)

func TestColdSetupPolicyBudgets(t *testing.T) {
	for _, tc := range []struct {
		name         string
		overallLimit time.Duration
		wantAuto     bool
	}{
		{name: "slow start leaves time to confirm automatic joining", wantAuto: true},
		{name: "setup overall deadline still caps later stages", overallLimit: 3 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := t.Context()
				if tc.overallLimit != 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, tc.overallLimit)
					defer cancel()
				}
				start := func(ctx context.Context) error {
					return waitForSetupPolicyFixture(ctx, 2700*time.Millisecond)
				}
				var policyApplied, autoApplied bool
				send := func(ctx context.Context, call cli.Call) (core.Result, error) {
					switch cmd := call.Command.(type) {
					case domain.AdapterSet:
						if err := waitForSetupPolicyFixture(ctx, 100*time.Millisecond); err != nil {
							return core.Result{}, err
						}
						policyApplied = cmd.Adapter == "claude" && cmd.Enabled != nil && *cmd.Enabled
					case domain.ChannelAuto:
						if waitForSetupPolicyFixture(ctx, 500*time.Millisecond) != nil {
							return core.Result{Status: core.ExitUnavailable, Code: core.CodeOutcomeUnknown}, nil
						}
						autoApplied = cmd.On && cmd.Initial
					default:
						t.Fatalf("unexpected setup command %T", cmd)
					}
					return core.Result{Status: core.ExitOK}, nil
				}
				confirmed, err := setSetupPolicy(ctx, start, send, "claude", true)
				if !confirmed || !policyApplied || autoApplied != tc.wantAuto {
					t.Fatalf("policy confirmed %t, applied %t; automatic joining applied %t, want %t; err %v",
						confirmed, policyApplied, autoApplied, tc.wantAuto, err)
				}
				if tc.wantAuto && err != nil {
					t.Fatalf("cold setup: %v", err)
				}
				if !tc.wantAuto && (err == nil || !strings.Contains(err.Error(), "adapter policy was applied") ||
					!strings.Contains(err.Error(), "outcome_unknown")) {
					t.Fatalf("expected the confirmed policy and unconfirmed automatic joining: %v", err)
				}
			})
		})
	}
}

func waitForSetupPolicyFixture(ctx context.Context, duration time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(duration):
		return nil
	}
}
