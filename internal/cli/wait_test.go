package cli

import (
	"context"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
)

type waitTimingTransport struct {
	fakeTransport
	start time.Time
	polls []time.Duration
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
