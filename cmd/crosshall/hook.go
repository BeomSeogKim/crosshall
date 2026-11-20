package main

import (
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/hook"
)

// The vendor must regain control even when stdin, stdout or a filesystem
// operation stalls. This process serves exactly one callback.
func runHook(entry time.Time, args []string) {
	// A vendor may close its stdout reader while a response is in flight.
	signal.Ignore(syscall.SIGPIPE)
	hook.Run(entry, args, os.Stdin, os.Stdout, func() (hook.Paths, error) {
		paths, err := resolvePaths()
		return hook.Paths{State: paths.state, Rendezvous: paths.rendezvous, Start: paths.launch(false).Start}, err
	})
}
