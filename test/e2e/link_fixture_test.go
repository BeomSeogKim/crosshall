//go:build e2e

package e2e

import (
	"debug/buildinfo"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/proc"
)

const linkFixtureTarget = "fixture-home.invalid"

type linkFixture struct {
	home, spoke *world
	ssh         string
}

func newLinkFixture(t *testing.T, homeBinary, spokeBinary string, homeEnv ...string) *linkFixture {
	t.Helper()
	f := &linkFixture{home: newWorld(t), spoke: newWorld(t)}
	f.home.executable, f.spoke.executable = homeBinary, spokeBinary
	f.home.extraEnv = append([]string{"CROSSHALL_EXPERIMENTAL_LINK=1", "DO_NOT_TRACK=1"}, homeEnv...)
	f.ssh = writeLocalSSH(t, f.home)
	f.spoke.extraEnv = []string{"CROSSHALL_EXPERIMENTAL_LINK=1", "DO_NOT_TRACK=1", "CROSSHALL_LINK_SSH=" + f.ssh}
	return f
}

func writeLocalSSH(t *testing.T, home *world) string {
	t.Helper()
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	path := filepath.Join(t.TempDir(), "ssh")
	var command strings.Builder
	for _, entry := range home.baseEnv() {
		command.WriteString(" " + quote(entry))
	}
	command.WriteString(" " + quote(home.binaryPath()))
	remote := "CROSSHALL_EXPERIMENTAL_LINK=1 " + quote(home.binaryPath())
	shim := fmt.Sprintf(`#!/bin/sh
set -eu
refuse() { printf '%%s\n' 'fixture ssh: expected the configured home command' >&2; exit 2; }
while [ "$#" -gt 0 ]; do
  case "$1" in
    -T) shift ;;
    -o) [ "$#" -ge 2 ] || refuse; shift 2 ;;
    --) shift; break ;;
    *) break ;;
  esac
done
[ "$#" -ge 2 ] && [ "$1" = %s ] || refuse
shift
if [ "$#" -eq 1 ]; then
  case "$1" in
    %s) printf '%%s\n' %s; exit 0 ;;
    %s)
      printf '%%s\n' "$$" > %s
      set -- link serve ;;
    %s*)
      key=${1#%s}
      key=${key%%%%\'*}
      case "$key" in ''|*[!0-9a-f]*) refuse ;; esac
      [ "${#key}" -eq 32 ] && [ "$1" = %s"$key"%s ] || refuse
      printf '%%s\n' "$key" >> %s
      set -- --key "$key" link enroll --name fixture-spoke
      if [ -f %s ]; then
        /usr/bin/env -i%s "$@" >/dev/null
        exit 1
      fi ;;
    *) refuse ;;
  esac
  exec /usr/bin/env -i%s "$@"
fi
[ "$1" = %s ] || refuse
shift
case "$#:$*" in
  '1:version'|'2:link serve') exec /usr/bin/env -i%s "$@" ;;
  *) refuse ;;
esac
`, quote(linkFixtureTarget),
		quote(`command -v crosshall || { test -x "$HOME/.local/bin/crosshall" && printf '%s\n' "$HOME/.local/bin/crosshall"; }`), quote(home.binaryPath()),
		quote(remote+" 'link' 'serve'"), quote(path+".pid"), quote(remote+" '--key' '"), quote(remote+" '--key' '"),
		quote(remote+" '--key' '"), quote("' 'link' 'enroll' '--name' 'fixture-spoke'"), quote(path+".enrolls"),
		quote(path+".drop-enrollment"), command.String(), command.String(), quote(home.binaryPath()), command.String())
	if err := os.WriteFile(path, []byte(shim), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

type linkFixtureDispatcher struct {
	w       *world
	process *os.Process
	start   int64
}

func linkDispatcher(w *world) linkFixtureDispatcher {
	w.t.Helper()
	data, err := os.ReadFile(filepath.Join(w.rendezvous(), "ready"))
	if err != nil {
		w.t.Fatal(err)
	}
	var ready struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal(data, &ready); err != nil || ready.PID <= 0 {
		w.t.Fatalf("decode dispatcher readiness %q: %v", data, err)
	}
	if pids := w.dispatcherPIDs(); !slices.Equal(pids, []int{ready.PID}) {
		w.t.Fatalf("fixture lock owners %v do not match ready pid %d", pids, ready.PID)
	}
	start, err := proc.StartOf(ready.PID)
	if err != nil || start.Identity <= 0 {
		w.t.Fatalf("read fixture dispatcher %d start: %+v, %v", ready.PID, start, err)
	}
	process, err := os.FindProcess(ready.PID)
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { _ = process.Release() })
	return linkFixtureDispatcher{w: w, process: process, start: start.Identity}
}

func (d linkFixtureDispatcher) alive() bool {
	return proc.ProcessAlive(int64(d.process.Pid), d.start)
}

func (d linkFixtureDispatcher) signal(signal syscall.Signal) {
	d.w.t.Helper()
	if !d.alive() {
		d.w.t.Fatalf("fixture dispatcher %d no longer has its captured start identity", d.process.Pid)
	}
	if err := d.process.Signal(signal); err != nil {
		d.w.t.Fatal(err)
	}
}

func (d linkFixtureDispatcher) stall() {
	d.w.t.Helper()
	d.w.t.Cleanup(func() {
		if d.alive() {
			_ = d.process.Signal(syscall.SIGCONT)
		}
	})
	d.signal(syscall.SIGSTOP)
}

func (d linkFixtureDispatcher) kill() {
	d.w.t.Helper()
	d.signal(syscall.SIGKILL)
	d.w.killAndWait(nil)
}

func withLinkBuildPairs(t *testing.T, check func(*testing.T, *linkFixture)) {
	t.Helper()
	older := os.Getenv(baseBinaryEnv)
	for _, pair := range []struct{ name, home, spoke string }{
		{"head-head", binary, binary},
		{"base-home", older, binary},
		{"base-spoke", binary, older},
	} {
		t.Run(pair.name, func(t *testing.T) {
			if pair.home == "" || pair.spoke == "" {
				t.Skip("set " + baseBinaryEnv + " to a previous build with -tags=testvendorhome")
			}
			if pair.name != "head-head" {
				requireFixtureBuild(t, older)
			}
			check(t, newLinkFixture(t, pair.home, pair.spoke))
		})
	}
}

func requireFixtureBuild(t *testing.T, path string) {
	t.Helper()
	if !filepath.IsAbs(path) {
		t.Fatalf("%s must be an absolute executable path", baseBinaryEnv)
	}
	info, err := buildinfo.ReadFile(path)
	if err != nil {
		t.Fatalf("read build info for %s: %v", baseBinaryEnv, err)
	}
	for _, setting := range info.Settings {
		if setting.Key == "-tags" && slices.Contains(strings.Fields(strings.ReplaceAll(setting.Value, ",", " ")), "testvendorhome") {
			return
		}
	}
	t.Fatalf("%s needs -tags=testvendorhome for isolated dispatcher cleanup", baseBinaryEnv)
}

func TestLinkFixtureSSHUsesHomeEnvironmentAndStdio(t *testing.T) {
	homeProbe := filepath.Join(t.TempDir(), "home's crosshall")
	if err := os.WriteFile(homeProbe, []byte(`#!/bin/sh
set -eu
[ "$#" -eq 2 ] && [ "$1" = link ] && [ "$2" = serve ]
printf '%s\n' "$HOME" "$CROSSHALL_STATE_DIR" "${SSH_AUTH_SOCK-unset}" "${CROSSHALL_LINK_SSH-unset}"
exec /bin/cat
`), 0700); err != nil {
		t.Fatal(err)
	}
	f := newLinkFixture(t, homeProbe, binary)
	cmd := exec.CommandContext(t.Context(), f.ssh, "-T", "-o", "BatchMode=yes", linkFixtureTarget, homeProbe, "link", "serve")
	cmd.Env = append(f.spoke.baseEnv(), "SSH_AUTH_SOCK=spoke-only")
	cmd.Stdin = strings.NewReader("fixture request\n")
	want := fmt.Sprintf("%s\n%s\nunset\nunset\nfixture request\n", f.home.home, f.home.state)
	if out, err := cmd.CombinedOutput(); err != nil || string(out) != want {
		t.Fatalf("local SSH stdio %q (%v), want %q", out, err, want)
	}
	wrongTarget := exec.CommandContext(t.Context(), f.ssh, "-T", "somewhere-else.invalid", homeProbe, "link", "serve")
	wrongTarget.Env = f.spoke.baseEnv()
	if out, err := wrongTarget.Output(); err == nil || len(out) != 0 {
		t.Fatalf("local SSH shim accepted an unknown target: %q, %v", out, err)
	}
}

func TestTwoDeviceFixture(t *testing.T) {
	withLinkBuildPairs(t, func(t *testing.T, f *linkFixture) {
		if f.home.state == f.spoke.state || f.home.rendezvous() == f.spoke.rendezvous() {
			t.Fatal("home and spoke fixtures share dispatcher state")
		}
		home, spoke := f.home.owner(), f.spoke.owner()
		home.must("owner", "start")
		spoke.must("owner", "start")
		home.must("owner", "channel", "create", "home-only")
		spoke.must("owner", "channel", "create", "spoke-only")
		if got := f.spoke.member().run("join", "home-only"); got.obj["code"] != "not_found" {
			t.Fatalf("unlinked spoke saw the home channel: %s", got.stdout)
		}
		if got := f.home.member().run("join", "spoke-only"); got.obj["code"] != "not_found" {
			t.Fatalf("unlinked home saw the spoke channel: %s", got.stdout)
		}

		version := home.mustPlain("version")
		ssh := exec.CommandContext(t.Context(), f.ssh, "-T", "-o", "BatchMode=yes", linkFixtureTarget, f.home.binaryPath(), "version")
		ssh.Env = f.spoke.baseEnv()
		if got, err := ssh.Output(); err != nil || string(got) != version {
			t.Fatalf("local SSH shim selected %q (%v), want home build %q", got, err, version)
		}
		homeDispatcher, spokeDispatcher := linkDispatcher(f.home), linkDispatcher(f.spoke)
		if homeDispatcher.process.Pid == spokeDispatcher.process.Pid {
			t.Fatal("home and spoke fixtures share a dispatcher process")
		}
		homeDispatcher.stall()
		spoke.must("owner", "channel", "create", "during-home-stall")
		homeDispatcher.signal(syscall.SIGCONT)
		spokeDispatcher.kill()
		if !homeDispatcher.alive() {
			t.Fatal("killing the spoke stopped the home dispatcher")
		}
		home.must("owner", "who", "--all")
		spoke.must("owner", "start")
		f.spoke.member().must("join", "during-home-stall")
	})
}
