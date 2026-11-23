package hook

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/proc"
	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// The stand-in host's PID and start identity.
const (
	standInPID   = 4242
	standInStart = 99
)

// standInHost makes the hook's host the given command for one test, a
// process whose start identity reads.
func standInHost(t *testing.T, exe string, args []string, ok bool) {
	t.Helper()
	previous, previousStart := hostCommand, processStart
	hostCommand = func() (int, string, []string, bool) { return standInPID, exe, args, ok }
	processStart = func(pid int) (proc.Start, error) {
		if pid != standInPID {
			return proc.Start{}, errors.New("no such process")
		}
		return proc.Start{Identity: standInStart, AtMS: 1}, nil
	}
	t.Cleanup(func() { hostCommand, processStart = previous, previousStart })
}

func environment(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		v, ok := values[name]
		return v, ok
	}
}

// A Codex hook run by the managed app server of its CODEX_HOME reports the
// daemon mode, the server's release and the server process; any other host
// reports in_process, and an unreadable one leaves the mode unverified
// (#236).
func TestCodexModeComesFromTheHooksHost(t *testing.T) {
	m, ok := loadManifest("codex")
	if !ok || m.Daemon == nil {
		t.Fatal("the Codex manifest names no daemon")
	}
	home := t.TempDir()
	user := t.TempDir()
	release := func(root, dir string) string {
		exe := filepath.Join(root, "packages", "app-server-daemon", "releases", dir, "bin", "codex")
		if err := os.MkdirAll(filepath.Dir(exe), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(exe, nil, 0o700); err != nil {
			t.Fatal(err)
		}
		return exe
	}
	server := release(home, "0.171.0-aarch64-apple-darwin")
	serving := []string{server, "app-server", "--listen", "unix://", "--analytics-default-enabled", "--managed-daemon"}
	link := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(home, link); err != nil {
		t.Fatal(err)
	}
	other := t.TempDir()
	for _, tc := range []struct {
		name          string
		exe           string
		args          []string
		readable      bool
		env           map[string]string
		version, mode string
	}{
		{"managed daemon of CODEX_HOME", server, serving, true, map[string]string{"CODEX_HOME": home}, "0.171.0", publish.ModeDaemon},
		{"CODEX_HOME through a symbolic link", server, serving, true, map[string]string{"CODEX_HOME": link}, "0.171.0", publish.ModeDaemon},
		{"default home under HOME", release(filepath.Join(user, ".codex"), "0.171.0-x86_64-unknown-linux-gnu"),
			[]string{"codex", "app-server", "--managed-daemon"}, true, map[string]string{"HOME": user}, "0.171.0", publish.ModeDaemon},
		{"pre-release of the daemon", release(home, "0.172.0-alpha.1-aarch64-apple-darwin"),
			[]string{"codex", "app-server", "--managed-daemon"}, true, map[string]string{"CODEX_HOME": home}, "unverified", publish.ModeDaemon},
		{"terminal client runs its own server", "/opt/homebrew/bin/codex", []string{"codex"}, true,
			map[string]string{"CODEX_HOME": home}, "unverified", publish.ModeInProcess},
		{"server without --managed-daemon", server, []string{server, "app-server", "--listen", "unix://"}, true,
			map[string]string{"CODEX_HOME": home}, "unverified", publish.ModeInProcess},
		{"daemon of another home", release(other, "0.171.0-aarch64-apple-darwin"), serving, true,
			map[string]string{"CODEX_HOME": home}, "unverified", publish.ModeInProcess},
		{"host not readable", "", nil, false, map[string]string{"CODEX_HOME": home}, "unverified", "unverified"},
		{"no home to check against", server, serving, true, map[string]string{}, "unverified", "unverified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			standInHost(t, tc.exe, tc.args, tc.readable)
			version, mode, host, served := daemonBuild(*m.Daemon, environment(tc.env))
			if version != tc.version || mode != tc.mode {
				t.Fatalf("build = %s %s, want %s %s", version, mode, tc.version, tc.mode)
			}
			want, wantHome := (*wire.Host)(nil), ""
			if mode == publish.ModeDaemon {
				want = &wire.Host{PID: standInPID, Start: standInStart, NS: proc.PIDNamespace()}
				wantHome = tc.env["CODEX_HOME"]
				if wantHome == "" {
					wantHome = filepath.Join(tc.env["HOME"], ".codex")
				}
				wantHome, _ = filepath.EvalSymlinks(wantHome)
			}
			if (host == nil) != (want == nil) || host != nil && *host != *want || served != wantHome {
				t.Fatalf("host = %+v in %q, want %+v in %q", host, served, want, wantHome)
			}
		})
	}
	// A server whose start does not read is not named: a PID alone could
	// later be another process.
	standInHost(t, server, serving, true)
	processStart = func(int) (proc.Start, error) { return proc.Start{}, errors.New("unreadable") }
	if _, mode, host, served := daemonBuild(*m.Daemon, environment(map[string]string{"CODEX_HOME": home})); mode != publish.ModeDaemon || host != nil || served != "" {
		t.Fatalf("unreadable start: mode %s host %+v home %q", mode, host, served)
	}
}

// Every Codex event carries the mode, so the launch's profile and its
// events' profiles stay equal.
func TestEveryCodexEventCarriesTheDaemonMode(t *testing.T) {
	m, _ := loadManifest("codex")
	home := t.TempDir()
	server := filepath.Join(home, "packages", "app-server-daemon", "releases", "0.171.0-aarch64-apple-darwin", "bin", "codex")
	if err := os.MkdirAll(filepath.Dir(server), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(server, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	standInHost(t, server, []string{server, "app-server", "--managed-daemon"}, true)
	for _, name := range []string{"SessionStart", "UserPromptSubmit", "Stop", "SessionEnd"} {
		spec, _ := m.event(name)
		body := `{"session_id":"fixture-codex","hook_event_name":"` + name + `","turn_id":"turn-1","source":"startup","reason":"other"}`
		e, err := parseEvent(strings.NewReader(body), m, spec, environment(map[string]string{"CODEX_HOME": home}))
		if err != nil || e.Mode != publish.ModeDaemon || e.VendorVersion != "0.171.0" {
			t.Fatalf("%s: mode %q version %q (%v)", name, e.Mode, e.VendorVersion, err)
		}
	}
}

// A queued Codex notice comes back as the member's own prompt; its hook
// turns it into a receipt and keeps any other prompt local.
func TestCodexPromptEchoesTheNoticeAsAReceipt(t *testing.T) {
	m, _ := loadManifest("codex")
	spec, _ := m.event("UserPromptSubmit")
	codex, _ := publish.Lookup("codex")
	line, _ := codex.Notice([]int64{3, 17})
	for prompt, want := range map[string]string{line: "publish:3 17", "please run crosshall read": ""} {
		raw, _ := json.Marshal(prompt)
		body := `{"session_id":"fixture-codex","hook_event_name":"UserPromptSubmit","turn_id":"turn-1","prompt":` + string(raw) + `}`
		e, err := parseEvent(strings.NewReader(body), m, spec, environment(nil))
		if err != nil || e.receipt != want {
			t.Fatalf("receipt of %q = %q (%v), want %q", prompt, e.receipt, err, want)
		}
	}
}
