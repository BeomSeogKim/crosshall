package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

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
