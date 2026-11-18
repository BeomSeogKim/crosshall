package client

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/spool"
)

func TestMemberStartSelectsDeadlineFromEntry(t *testing.T) {
	for _, tc := range []struct {
		name    string
		spawned bool
		err     error
		budget  time.Duration
	}{
		{name: "warm", budget: time.Second},
		{name: "home state unavailable", err: errors.New("permission denied"), budget: time.Second},
		{name: "cold", spawned: true, budget: 3 * time.Second},
		{name: "spawned but not ready", spawned: true, err: errors.New("readiness failed"), budget: 3 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := time.Now().Add(-100 * time.Millisecond)
			transport := New(spool.Client{})
			transport.EnableMemberStart(entry, func(ctx context.Context) (bool, error) {
				deadline, ok := ctx.Deadline()
				if !ok || !deadline.Equal(entry.Add(3*time.Second)) {
					t.Fatalf("launcher got deadline %v, want three seconds from entry", deadline)
				}
				return tc.spawned, tc.err
			})
			ctx, cancel := transport.memberCommandContext(t.Context())
			defer cancel()
			deadline, ok := ctx.Deadline()
			if !ok || !deadline.Equal(entry.Add(tc.budget)) || ctx.Err() != nil {
				t.Fatalf("member deadline %v, err %v; want %v from entry", deadline, ctx.Err(), tc.budget)
			}
		})
	}
}

func TestMemberStartSkipsExpiredWarmBudget(t *testing.T) {
	transport := New(spool.Client{})
	transport.EnableMemberStart(time.Now().Add(-2*time.Second), func(context.Context) (bool, error) {
		t.Fatal("expired invocation attempted to launch a dispatcher")
		return false, nil
	})
	ctx, cancel := transport.memberCommandContext(t.Context())
	defer cancel()
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("member context error = %v; want deadline exceeded", ctx.Err())
	}
}
