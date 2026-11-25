package setup

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/hook"
)

// HostBuild is a vendor executable setup recorded with the owner's consent:
// its real path, file identity, size, modification time and version.
type HostBuild struct {
	Executable string `json:"executable"`
	Version    string `json:"version"`
	Identity   string `json:"file_id"`
	Size       int64  `json:"size"`
	ModifiedNS int64  `json:"modified_ns"`
}

// RecordedBuild is the build setup last recorded for adapter.
func RecordedBuild(stateRoot, adapter string) (HostBuild, bool) {
	j, _, err := readJournal(stateRoot)
	if err != nil {
		return HostBuild{}, false
	}
	b, ok := j.Builds[adapter]
	if !ok || !filepath.IsAbs(b.Executable) {
		return HostBuild{}, false
	}
	return HostBuild{Executable: b.Executable, Version: b.Version, Identity: b.Identity, Size: b.Size, ModifiedNS: b.ModifiedNS}, true
}

// Current reports whether the file at the build's path is still the one
// recorded.
func (b HostBuild) Current() bool {
	info, err := os.Stat(b.Executable)
	return err == nil && info.Mode().IsRegular() && configIdentity(info) == b.Identity &&
		info.Size() == b.Size && info.ModTime().UnixNano() == b.ModifiedNS
}

// Reprobe reads the version of the executable at a recorded build's path,
// never another path, as setup does. It reports false when the file
// changes during the probe or its version does not parse.
func Reprobe(ctx context.Context, adapter string, recorded HostBuild, timeout time.Duration) (HostBuild, bool) {
	var homeEnv string
	for _, spec := range hook.InstallSpecs() {
		if spec.Adapter == adapter {
			homeEnv = spec.HomeEnv
		}
	}
	stat := func() (HostBuild, bool) {
		info, err := os.Stat(recorded.Executable)
		if err != nil || !info.Mode().IsRegular() {
			return HostBuild{}, false
		}
		return HostBuild{Executable: recorded.Executable, Identity: configIdentity(info), Size: info.Size(), ModifiedNS: info.ModTime().UnixNano()}, true
	}
	before, ok := stat()
	if !ok {
		return HostBuild{}, false
	}
	before.Version = probeVersion(ctx, homeEnv, recorded.Executable, timeout)
	after, ok := stat()
	if !ok || before.Version == unknown || after.Identity != before.Identity || after.Size != before.Size || after.ModifiedNS != before.ModifiedNS {
		return HostBuild{}, false
	}
	return before, true
}
