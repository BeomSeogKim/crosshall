package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/hook"
	"github.com/BeomSeogKim/crosshall/internal/spool"
)

// The vendor must regain control even when stdin, stdout or a filesystem
// operation stalls. This process serves exactly one callback.
func runHook(entry time.Time, args []string) {
	// A vendor may close its stdout reader while a response is in flight.
	signal.Ignore(syscall.SIGPIPE)
	ctx, cancel := context.WithDeadline(context.Background(), entry.Add(hook.Budget))
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		paths, err := resolvePaths()
		if err != nil {
			return
		}
		hook.Run(ctx, entry, spool.Client{Rendezvous: paths.rendezvous}, args, os.Stdin, os.Stdout)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}
