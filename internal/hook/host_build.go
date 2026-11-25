package hook

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/BeomSeogKim/crosshall/internal/ident"
)

// HostBuildsName is the rendezvous file the dispatcher publishes from the
// builds setup recorded: "<adapter> <version> <file id> <size> <mtime ns>
// <real path>" per line, only while the file at the path is still that
// build.
const HostBuildsName = "builds"

var hostBuildVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(?:[-+][0-9A-Za-z.-]+)?$`)

// FileID names a file by device and inode, as setup's records do.
func FileID(info os.FileInfo) string {
	stat := info.Sys().(*syscall.Stat_t)
	return fmt.Sprintf("%x:%x", uint64(stat.Dev), uint64(stat.Ino))
}

// HostBuildLine renders one HostBuildsName line.
func HostBuildLine(adapter, version, id string, size, modifiedNS int64, path string) string {
	return fmt.Sprintf("%s %s %s %d %d %s\n", adapter, version, id, size, modifiedNS, path)
}

// hostBuild is the version and mode of the executable running this hook,
// "unverified" for what it cannot name: no recorded build for its exact
// file, or no mode the manifest declares.
func hostBuild(rv *os.Root, m manifest) (version, mode string) {
	version, mode = "unverified", "unverified"
	_, exe, args, ok := hostCommand()
	if !ok {
		return version, mode
	}
	if m.HostModeDefault != "" {
		mode = m.HostModeDefault
	}
	for name, flags := range m.HostModes {
		if len(args) > 1 && slices.ContainsFunc(args[1:], func(a string) bool { return slices.Contains(flags, a) }) {
			mode = name
		}
	}
	data, err := ident.ReadPrivateFile(rv, HostBuildsName, 16<<10)
	if err != nil {
		return version, mode
	}
	path := realPath(exe)
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.SplitN(line, " ", 6)
		if len(f) != 6 || f[0] != m.Adapter || f[5] != path || !hostBuildVersion.MatchString(f[1]) {
			continue
		}
		size, sizeErr := strconv.ParseInt(f[3], 10, 64)
		modified, modErr := strconv.ParseInt(f[4], 10, 64)
		info, err := os.Stat(path)
		if sizeErr == nil && modErr == nil && err == nil && info.Mode().IsRegular() &&
			FileID(info) == f[2] && info.Size() == size && info.ModTime().UnixNano() == modified {
			version = f[1]
		}
		break
	}
	return version, mode
}

// BuildFromHost reports that adapter's hooks take their build from the
// executable that runs them (manifest build_from_host).
func BuildFromHost(adapter string) bool {
	m, ok := loadManifest(adapter)
	return ok && m.BuildFromHost
}
