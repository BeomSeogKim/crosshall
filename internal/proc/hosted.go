package proc

import (
	"os"
	"path"
	"slices"
	"strings"
)

// shells may stand between an agent process and a command it runs.
var shells = map[string]bool{"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "fish": true}

// HostedBy reports whether pid is this process's nearest ancestor that is not
// a shell: the agent process that runs this command, not a session nested
// under it.
func HostedBy(pid int) bool {
	p := os.Getppid()
	for range 16 {
		if p == pid {
			return true
		}
		name, parent, err := processParent(p)
		if err != nil || !shells[strings.TrimPrefix(path.Base(name), "-")] || parent <= 1 {
			return false
		}
		p = parent
	}
	return false
}

// Host finds the agent process that hosts this command: the process envPID
// names when it is an ancestor, or, when envPID is zero, the nearest ancestor
// whose command name is in names. Any process may stand between, at most 16
// levels: a command run through a wrapper still finds the agent above it.
func Host(envPID int, names []string) (int, Start, bool) {
	p := os.Getppid()
	for range 16 {
		if p <= 1 {
			break
		}
		name, parent, err := processParent(p)
		if err != nil {
			break
		}
		if envPID > 0 && p == envPID || envPID <= 0 && slices.Contains(names, strings.TrimPrefix(path.Base(name), "-")) {
			start, err := processStart(p)
			if err != nil || start.Identity <= 0 {
				break
			}
			return p, start, true
		}
		p = parent
	}
	return 0, Start{}, false
}

// HostCommand names the process that runs this command, past any shells in
// between: its executable path and arguments. ok is false when they cannot
// be read.
func HostCommand() (exe string, args []string, ok bool) {
	p := os.Getppid()
	for range 16 {
		if p <= 1 {
			return "", nil, false
		}
		name, parent, err := processParent(p)
		if err != nil {
			return "", nil, false
		}
		if shells[strings.TrimPrefix(path.Base(name), "-")] {
			p = parent
			continue
		}
		exe, args, err := processCommand(p)
		return exe, args, err == nil
	}
	return "", nil, false
}
