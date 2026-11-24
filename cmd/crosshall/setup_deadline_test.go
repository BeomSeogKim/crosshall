package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/cli"
	"github.com/BeomSeogKim/crosshall/internal/core"
)

func TestApplyWatchdogExitsWhenWorkCannotObserveCancellation(t *testing.T) {
	const child = "CROSSHALL_TEST_SETUP_WATCHDOG_CHILD"
	if action := os.Getenv(child); action != "" {
		ctx, stop := applyDeadline(t.Context(), action, 10*time.Millisecond)
		defer stop()
		<-ctx.Done()
		if context.Cause(ctx).Error() != applyTimeoutMessage(action) {
			t.Fatalf("%s deadline cause: %v", action, context.Cause(ctx))
		}
		time.Sleep(12 * time.Second)
		t.Fatal("the watchdog did not exit")
	}
	for _, action := range []string{"setup", "uninstall"} {
		t.Run(action, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestApplyWatchdogExitsWhenWorkCannotObserveCancellation$")
			cmd.Env = append(os.Environ(), child+"="+action)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != int(core.ExitError) || string(out) != applyTimeoutMessage(action)+"\n" {
				t.Fatalf("watchdog child: %v, %s", err, strings.TrimSpace(string(out)))
			}
		})
	}
}

func TestEachApplyNamesItsOwnRetry(t *testing.T) {
	setup, uninstall := applyTimeoutMessage("setup"), applyTimeoutMessage("uninstall")
	if !strings.HasPrefix(setup, "setup apply timed out.") || !strings.HasPrefix(uninstall, "uninstall timed out.") ||
		!strings.Contains(uninstall, "crosshall uninstall") {
		t.Fatalf("timeout messages: %q, %q", setup, uninstall)
	}
}

func TestAPolicyCallCutByTheDeadlineStillNamesIt(t *testing.T) {
	unsent := core.Result{Status: core.ExitUnavailable, Code: core.CodeUnsent}
	for _, tc := range []struct {
		name    string
		enabled bool
		results []core.Result
	}{
		{"policy update", false, []core.Result{unsent}},
		{"automatic joining", true, []core.Result{{Status: core.ExitOK}, unsent}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, expired := range []bool{true, false} {
				ctx, cancel := context.WithCancel(t.Context())
				if expired {
					cancel()
				}
				calls := 0
				// The transport reports an unsent call as unavailable and drops its error.
				do := func(context.Context, cli.Call) (core.Result, error) {
					calls++
					return tc.results[calls-1], nil
				}
				var ready bool
				err := policySetter(func(context.Context) error { return nil }, do, &ready)(ctx, "codex", tc.enabled)
				cancel()
				if err == nil || calls != len(tc.results) || errors.Is(err, context.Canceled) != expired {
					t.Fatalf("expired %v: %d calls, error %v", expired, calls, err)
				}
			}
		})
	}
}
