package ident

import "github.com/BeomSeogKim/crosshall/internal/proc"

// LaunchProcess returns the process part of a bound launch reference; see
// proc.LaunchProcess.
func LaunchProcess(ref string) (string, bool) { return proc.LaunchProcess(ref) }

// LaunchAlive reports whether a bound launch still runs; see
// proc.LaunchAlive.
func LaunchAlive(ref string, startedHookAtMS int64) bool {
	return proc.LaunchAlive(ref, startedHookAtMS)
}
