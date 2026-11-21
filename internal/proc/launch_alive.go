// Package proc identifies local processes: their start times, the launch a
// reference names, and the kernel peer of a Unix socket. It imports nothing
// from crosshall, so any package may use it.
package proc

import (
	"errors"
	"math"
	"strconv"
	"strings"
)

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
// is a running process (not a zombie) that started no later than the
// launch's first hook. A process starts before its own first hook, so a
// reused PID, born after that hook, does not count.
func LaunchAlive(ref string, startedHookAtMS int64) bool {
	process, ok := LaunchProcess(ref)
	if !ok {
		return false
	}
	pid, _ := strconv.Atoi(strings.TrimPrefix(process, "pid-"))
	start, err := processStartMS(pid)
	return err == nil && start <= startedHookAtMS
}

// LaunchIdentity returns a live launch's PID and kernel start identity.
// The first hook time is an upper bound on the process start.
func LaunchIdentity(ref string, startedHookAtMS int64) (pid int64, identity int64) {
	process, ok := LaunchProcess(ref)
	if !ok || len(ref) <= len(process)+1 || startedHookAtMS <= 0 {
		return 0, 0
	}
	pid, err := strconv.ParseInt(strings.TrimPrefix(process, "pid-"), 10, 32)
	if err != nil {
		return 0, 0
	}
	start, err := processStart(int(pid))
	if err != nil || start.Identity <= 0 || start.AtMS <= 0 || start.AtMS > startedHookAtMS {
		return 0, 0
	}
	return pid, start.Identity
}

// errProcessGone means the PID is absent or names a zombie.
var errProcessGone = errors.New("process gone")

// ProcessGone reports proof that the process with pid and start identity
// (see Start) no longer runs: no process has the PID, it is a zombie, or
// the PID belongs to a process with another start. An error that proves
// neither (an unsupported platform, an unreadable process) is not proof.
func ProcessGone(pid, identity int64) bool {
	if pid <= 0 || pid > math.MaxInt32 || identity <= 0 {
		return false
	}
	start, err := processStart(int(pid))
	return errors.Is(err, errProcessGone) || start.Identity > 0 && start.Identity != identity
}
