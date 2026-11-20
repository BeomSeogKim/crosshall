package ident

import (
	"strconv"
	"strings"
)

// launchClockSlackMS covers the whole-second boot time behind Linux start times.
const launchClockSlackMS = 1000

// LaunchProcess returns the process part of a bound Claude Code launch
// reference ("pid-<pid>-<start digest>"): launches of one process share it.
func LaunchProcess(ref string) (string, bool) {
	rest, ok := strings.CutPrefix(ref, "pid-")
	if !ok {
		return "", false
	}
	digits, _, _ := strings.Cut(rest, "-")
	if pid, err := strconv.Atoi(digits); err != nil || pid <= 0 || strconv.Itoa(pid) != digits {
		return "", false
	}
	return "pid-" + digits, true
}

// LaunchAlive reports whether a bound Claude Code launch still runs: its PID
// is live and that process started no later than the launch's first hook. A
// reused PID belongs to a later process.
func LaunchAlive(ref string, startedHookAtMS int64) bool {
	process, ok := LaunchProcess(ref)
	if !ok {
		return false
	}
	pid, _ := strconv.Atoi(strings.TrimPrefix(process, "pid-"))
	start, err := processStartMS(pid)
	return err == nil && start <= startedHookAtMS+launchClockSlackMS
}
