package hook

import (
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/BeomSeogKim/crosshall/internal/proc"
	"github.com/BeomSeogKim/crosshall/internal/publish"
)

// hostCommand is proc.HostCommand; tests stand in for it.
var hostCommand = proc.HostCommand

// releaseVersion is a release directory's name: the version, then the
// platform triple. A pre-release stays unverified.
var releaseVersion = regexp.MustCompile(`^([0-9]+\.[0-9]+\.[0-9]+)(-(aarch64|x86_64)-[a-z0-9_-]+)?$`)

// daemonBuild reads a session's mode from the process that runs its hook:
// the managed server, an executable in a release directory of the vendor's
// home run with the server's arguments, gives publish.ModeDaemon and that release's
// version. Any other readable host gives publish.ModeInProcess, an unreadable one
// leaves both unverified.
func daemonBuild(d daemonSpec, lookup func(string) (string, bool)) (version, mode string) {
	exe, args, ok := hostCommand()
	if !ok {
		return "unverified", "unverified"
	}
	home, ok := lookup(d.HomeEnv)
	if !ok || !filepath.IsAbs(home) {
		user, found := lookup("HOME")
		if !found || !filepath.IsAbs(user) {
			return "unverified", "unverified"
		}
		home = filepath.Join(user, d.HomeDefault)
	}
	releases := filepath.Join(realPath(home), d.Releases)
	rel, err := filepath.Rel(releases, realPath(exe))
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") || len(args) == 0 {
		return "unverified", publish.ModeInProcess
	}
	for _, want := range d.Args {
		if !slices.Contains(args[1:], want) {
			return "unverified", publish.ModeInProcess
		}
	}
	release, _, _ := strings.Cut(rel, string(filepath.Separator))
	version = "unverified"
	if match := releaseVersion.FindStringSubmatch(release); match != nil {
		version = match[1]
	}
	return version, publish.ModeDaemon
}

// realPath resolves symbolic links where it can: a home under /tmp is
// /private/tmp to the kernel on macOS.
func realPath(p string) string {
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return filepath.Clean(p)
}
