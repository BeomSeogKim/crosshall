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

func TestSetupApplyWatchdogExitsWhenWorkCannotObserveCancellation(t *testing.T) {
	const child = "CROSSHALL_TEST_SETUP_WATCHDOG_CHILD"
	if os.Getenv(child) == "1" {
		ctx, stop := setupApplyDeadline(t.Context(), 10*time.Millisecond)
		defer stop()
		<-ctx.Done()
		if context.Cause(ctx).Error() != setupApplyTimeoutMessage {
			t.Fatalf("setup deadline cause: %v", context.Cause(ctx))
		}
		time.Sleep(12 * time.Second)
		t.Fatal("the watchdog did not exit")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestSetupApplyWatchdogExitsWhenWorkCannotObserveCancellation$")
	cmd.Env = append(os.Environ(), child+"=1")
	out, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != int(core.ExitError) || string(out) != setupApplyTimeoutMessage+"\n" {
		t.Fatalf("watchdog child: %v, %s", err, strings.TrimSpace(string(out)))
	}
}
