package cli

import (
	"context"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
)

type waitTimingTransport struct {
	fakeTransport
	start  time.Time
	stopAt time.Duration
	polls  []time.Duration
}

func (f *waitTimingTransport) DispatcherRunning() bool {
	return f.stopAt == 0 || time.Since(f.start) < f.stopAt
}

func (f *waitTimingTransport) Do(ctx context.Context, call Call) (core.Result, error) {
	f.polls = append(f.polls, time.Since(f.start))
	return f.fakeTransport.Do(ctx, call)
}

func TestWaitPollsBackOffToOncePerSecond(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		transport := &waitTimingTransport{
			fakeTransport: fakeTransport{result: okResult(t, domain.WaitSnapshot{Cursor: domain.WaitCursor{BindingID: 1}})},
			start:         time.Now(),
		}
		_, stderr, code := run(t, Env{Transport: transport}, "wait", "--timeout", "4s")
		if elapsed := time.Since(transport.start); code != core.ExitWaitTimeout || elapsed != 4*time.Second {
			t.Fatalf("exit %d at %s, stderr %q", code, elapsed, stderr)
		}
		want := []time.Duration{0, 100 * time.Millisecond, 300 * time.Millisecond, 700 * time.Millisecond,
			1500 * time.Millisecond, 2500 * time.Millisecond, 3500 * time.Millisecond}
		if !slices.Equal(transport.polls, want) {
			t.Fatalf("polls at %v, want %v", transport.polls, want)
		}
	})
}

func TestTimedWaitChecksDispatcherWhileBackedOff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		transport := &waitTimingTransport{
			fakeTransport: fakeTransport{result: okResult(t, domain.WaitSnapshot{Cursor: domain.WaitCursor{BindingID: 1}})},
			start:         time.Now(),
			stopAt:        3750 * time.Millisecond,
		}
		stdout, _, code := run(t, Env{Transport: transport}, "--json", "wait", "--timeout", "4s")
		if code != core.ExitUnavailable || !strings.Contains(stdout, `"code":"dispatcher_unavailable"`) {
			t.Fatalf("exit %d, stdout %q", code, stdout)
		}
		if last := transport.polls[len(transport.polls)-1]; last >= transport.stopAt {
			t.Fatalf("polled at %v after the dispatcher stopped at %v", last, transport.stopAt)
		}
	})
}
