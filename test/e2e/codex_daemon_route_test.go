//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/domain"
)

// codexDaemonSource stands in for Codex's managed app server: it runs the
// command after "--" as its child, the way the server runs a hook.
const codexDaemonSource = `package main

import (
	"errors"
	"os"
	"os/exec"
)

func main() {
	args := os.Args[1:]
	for i, arg := range args {
		if arg == "--" {
			args = args[i+1:]
			break
		}
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			os.Exit(exit.ExitCode())
		}
		os.Exit(1)
	}
}
`

func TestCodexJoinShowsWakeGuidance(t *testing.T) {
	t.Run("no hook start", func(t *testing.T) {
		f := newSetupWorld(t)
		f.apply("", "setup", "codex", "--apply")
		f.enrollmentRepo()
		member := &participant{w: f.world, extra: []string{"CODEX_THREAD_ID=codex-join-without-hook"}}
		const fix = "Check /hooks and trust the crosshall hooks if needed, then reopen Codex in its default managed app server."
		if first := member.mustPlain("join", "setup-check", "--as", "codex"); strings.Count(first, fix) != 1 ||
			!strings.Contains(first, "codex daemon not confirmed") {
			t.Fatalf("first hand join printed %q, want the reason and how to check hook trust", first)
		}
		if again := member.mustPlain("join", "setup-check"); strings.Contains(again, fix) {
			t.Fatalf("repeated hand join printed the guidance again: %q", again)
		}
	})
	t.Run("installed hook without daemon", func(t *testing.T) {
		f := newSetupWorld(t)
		f.apply("", "setup", "codex", "--apply")
		repo := f.enrollmentRepo()
		const line = "The hall cannot wake this session (codex runs without its daemon). Reopen Codex in its default managed app server."
		deadline := time.Now().Add(fixtureEventTimeout)
		for {
			out := f.invokeHook(repo, "codex", "SessionStart", `"source":"startup"`)
			if strings.Count(out, line) == 1 {
				break
			}
			if out != "" || !time.Now().Before(deadline) {
				t.Fatalf("installed start printed %q, want %q; %s", out, line, f.enrollmentEvidence("codex"))
			}
			f.waitEnrollmentPackets("codex", "SessionStart", deadline)
			time.Sleep(20 * time.Millisecond)
		}
		f.waitEnrollmentPackets("codex", "SessionStart", deadline)
		var told int
		if err := f.db().QueryRow(`SELECT b.reach_told FROM binding b JOIN principal_key k ON k.principal_id = b.principal_id
			WHERE k.vendor = 'codex' AND k.key = ?`, setupSessionPrefix+"codex").Scan(&told); err != nil || told != 0 {
			t.Fatalf("the printed wake guidance has receipt %d, %v, want 0", told, err)
		}
		if prompt := f.invokeHook(repo, "codex", "UserPromptSubmit", `"turn_id":"after-guidance"`); strings.Contains(prompt, line) {
			t.Fatalf("first prompt repeated the guidance confirmed at start: %q", prompt)
		}
	})
}

// The installed Codex hook opens a route only when Codex's managed app
// server runs it; a start under any other process records why the session
// cannot be reached (#236).
func TestCodexRouteOpensOnlyUnderTheManagedDaemon(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("requires reading a process's executable and arguments")
	}
	f := newSetupWorld(t)
	f.apply("", "setup", "codex", "--apply")
	repo := f.enrollmentRepo()
	daemon := buildCodexDaemon(t, filepath.Dir(f.paths["codex"]))
	codexHome, err := filepath.EvalSymlinks(filepath.Dir(f.paths["codex"]))
	if err != nil {
		t.Fatal(err)
	}
	// A shell that runs more than the hook stays between it and the server,
	// as a hook command of several commands would; one that runs only the
	// hook may replace itself with it.
	start := func(source string, underDaemon, keepShell bool) (pid int) {
		t.Helper()
		cmd := f.installedHookCommand(t.Context(), repo, "codex", "SessionStart", fmt.Sprintf(`"source":%q`, source))
		if keepShell {
			cmd.Args[len(cmd.Args)-1] += "; :"
		}
		if underDaemon {
			wrapped := exec.CommandContext(t.Context(), daemon, append([]string{"app-server", "--managed-daemon", "--"}, cmd.Args...)...)
			wrapped.Dir, wrapped.Env, wrapped.Stdin = cmd.Dir, cmd.Env, cmd.Stdin
			cmd = wrapped
		}
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil || stderr.Len() != 0 {
			t.Fatalf("codex SessionStart %s: %v, stderr %q", source, err, stderr.String())
		}
		return cmd.Process.Pid
	}
	db := f.db()
	// A start may first bring the dispatcher up; it is sent again until
	// the route shows it.
	startUntil := func(source string, underDaemon, keepShell bool, want string) {
		t.Helper()
		var mode, version, home string
		var pid, host int
		for deadline := time.Now().Add(fixtureEventTimeout); ; time.Sleep(100 * time.Millisecond) {
			pid = start(source, underDaemon, keepShell)
			err := db.QueryRow(`SELECT COALESCE(r.mode, ''), json_extract(l.profile, '$.vendor_version'), COALESCE(r.host_pid, 0), COALESCE(r.vendor_home, '')
				FROM publish_route r JOIN adapter_launch l ON l.principal_id = r.principal_id AND l.launch_ref = r.launch_ref
				WHERE r.adapter = 'codex' AND r.state = 'active'`).Scan(&mode, &version, &host, &home)
			if err == nil && mode == want {
				break
			}
			if !time.Now().Before(deadline) {
				t.Fatalf("codex route mode %q (version %q, %v), want %q; %s", mode, version, err, want, f.enrollmentEvidence("codex"))
			}
		}
		if want == "daemon" && (version != "0.171.0" || host != pid || home != codexHome) {
			t.Fatalf("daemon launch version %q host %d in %q, want the server's release, process %d and %q", version, host, home, pid, codexHome)
		}
		if want != "daemon" && (host != 0 || home != "") {
			t.Fatalf("a %s launch named a server %d in %q", want, host, home)
		}
	}

	evidence, err := json.Marshal(domain.AdapterEvidence{Vendor: "codex", VendorVersion: "0.171.0",
		Platform: runtime.GOOS + "-" + runtime.GOARCH, Mode: "daemon", ManifestRev: "codex-1", RunAtMS: time.Now().UnixMilli(),
		Results: []domain.EvidenceResult{{Cap: "publish", Level: "candidate", Check: "fixture daemon for launch classification"}}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(f.home, "codex-route-evidence.json")
	if err := os.WriteFile(path, evidence, 0600); err != nil {
		t.Fatal(err)
	}
	f.owner().must("owner", "adapter", "evidence", path)
	startUntil("startup", true, true, "daemon")
	if out := f.owner().mustPlain("owner", "who", "--all"); !strings.Contains(out, "publish: candidate (version 0.171.0 exact evidence is candidate)") {
		t.Fatalf("a daemon session should honor candidate evidence for its exact build:\n%s", out)
	}
	startUntil("resume", false, true, "in_process")
	if out := f.owner().mustPlain("owner", "who", "--all"); !strings.Contains(out, "publish: candidate (codex runs without its daemon)") {
		t.Fatalf("an in-process session should say why it cannot be reached:\n%s", out)
	}
	startUntil("resume", true, false, "daemon")
}

// buildCodexDaemon builds the stand-in server where Codex installs its
// managed server: a release directory under CODEX_HOME.
func buildCodexDaemon(t *testing.T, codexHome string) string {
	t.Helper()
	triple := map[string]string{"darwin": "apple-darwin", "linux": "unknown-linux-gnu"}[runtime.GOOS]
	arch := map[string]string{"arm64": "aarch64", "amd64": "x86_64"}[runtime.GOARCH]
	exe := filepath.Join(codexHome, "packages", "app-server-daemon", "releases", "0.171.0-"+arch+"-"+triple, "bin", "codex")
	if err := os.MkdirAll(filepath.Dir(exe), 0o700); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "main.go")
	if err := os.WriteFile(src, []byte(codexDaemonSource), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	if raw, err := exec.CommandContext(ctx, "go", "build", "-o", exe, src).CombinedOutput(); err != nil {
		t.Fatalf("build the stand-in server: %v\n%s", err, raw)
	}
	return exe
}
