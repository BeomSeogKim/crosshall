package proc

import (
	"os"
	"path"
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
