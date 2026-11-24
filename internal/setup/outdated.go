package setup

import (
	"fmt"

	"github.com/BeomSeogKim/crosshall/internal/hook"
)

// markOutdated records the revision setup installed when its hook entries
// are still on disk, and marks them outdated when that revision is not the
// one this binary installs and a hook it installs is missing. A revision
// that changed no installed entry is not outdated.
func (s *InstallStatus) markOutdated(spec hook.InstallSpec, installedRev string) {
	if s.recorded == 0 || installedRev == "" {
		return
	}
	s.InstalledRev = installedRev
	if installedRev != spec.Revision && s.Installed < s.Intended {
		s.outdated = fmt.Sprintf("The %s hooks on disk were installed for %s; this crosshall installs %s. Run crosshall setup %s.",
			statusAdapterName(spec.Adapter), installedRev, spec.Revision, spec.Adapter)
	}
}

// OutdatedHooks is the sentence that names the step when an earlier
// crosshall installed the adapter's hooks, and empty otherwise.
func (s InstallStatus) OutdatedHooks() string { return s.outdated }

// OutdatedHooks returns that sentence for every adapter it applies to.
func OutdatedHooks(stateRoot string) []string {
	var lines []string
	for _, status := range Inspect(stateRoot) {
		if line := status.OutdatedHooks(); line != "" {
			lines = append(lines, line)
		}
	}
	return lines
}
