package hook

import (
	"strconv"
	"strings"

	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/proc"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// MemberHost names the agent process that runs a member command, so that
// the hall can tell when the session ended: for crosshall run the process it
// replaced itself with, or an ancestor the adapter manifest identifies.
func MemberHost(id ident.Identity, lookup func(string) (string, bool)) *wire.Host {
	var envPID, want int64
	var names []string
	if id.Vendor == "route" {
		value, _ := lookup(ident.HostVariable)
		pid, start, ok := strings.Cut(value, ":")
		if !ok {
			return nil
		}
		var err1, err2 error
		envPID, err1 = strconv.ParseInt(pid, 10, 32)
		want, err2 = strconv.ParseInt(start, 10, 64)
		if err1 != nil || err2 != nil || want <= 0 {
			return nil
		}
	} else {
		m, ok := loadManifest(id.Vendor)
		if !ok {
			return nil
		}
		if m.LaunchEnv != "" {
			value, _ := lookup(m.LaunchEnv)
			var err error
			if envPID, err = strconv.ParseInt(value, 10, 32); err != nil || envPID <= 1 {
				return nil
			}
		} else {
			names = m.HostNames
		}
	}
	if envPID <= 1 && len(names) == 0 {
		return nil
	}
	pid, start, ok := proc.Host(int(envPID), names)
	if !ok || want != 0 && start.Identity != want {
		return nil
	}
	return &wire.Host{PID: int64(pid), Start: start.Identity, NS: proc.PIDNamespace()}
}
