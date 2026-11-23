// Package proc identifies local processes: their start times, the launch a
// reference names, and the kernel peer of a Unix socket. It imports nothing
// from crosshall, so any package may use it.
package proc

import (
	"errors"
	"math"
	"runtime"
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

// ProbeLaunch reports the identity of a live launch or positive evidence that
// it has ended. The caller must compare historical PIDs in their original namespace.
func ProbeLaunch(ref string, startedHookAtMS int64) (pid, identity int64, gone bool) {
	process, ok := LaunchProcess(ref)
	if !ok || len(ref) <= len(process)+1 {
		return 0, 0, false
	}
	pid, err := strconv.ParseInt(strings.TrimPrefix(process, "pid-"), 10, 32)
	if err != nil {
		return 0, 0, false
	}
	start, err := probeProcessStart(int(pid))
	if errors.Is(err, errProcessGone) {
		return pid, 0, true
	}
	if err != nil || start.Identity <= 0 || start.AtMS <= 0 || startedHookAtMS <= 0 {
		return 0, 0, false
	}
	if start.AtMS > startedHookAtMS {
		// Linux estimates the boot time from the wall clock and uptime, so a
		// clock step can make a live process appear newer than its hook.
		if runtime.GOOS == "darwin" {
			return pid, 0, true
		}
		return 0, 0, false
	}
	return pid, start.Identity, false
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
	start, err := probeProcessStart(int(pid))
	return errors.Is(err, errProcessGone) || start.Identity > 0 && start.Identity != identity
}

func ProcessAlive(pid, identity int64) bool {
	if pid <= 0 || pid > math.MaxInt32 || identity <= 0 {
		return false
	}
	start, err := probeProcessStart(int(pid))
	return err == nil && start.Identity == identity
}
