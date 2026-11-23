//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
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
		var mode, version string
		var pid, host int
		for deadline := time.Now().Add(fixtureEventTimeout); ; time.Sleep(100 * time.Millisecond) {
			pid = start(source, underDaemon, keepShell)
			err := db.QueryRow(`SELECT COALESCE(r.mode, ''), json_extract(l.profile, '$.vendor_version'), COALESCE(r.host_pid, 0)
				FROM publish_route r JOIN adapter_launch l ON l.principal_id = r.principal_id AND l.launch_ref = r.launch_ref
				WHERE r.adapter = 'codex' AND r.state = 'active'`).Scan(&mode, &version, &host)
			if err == nil && mode == want {
				break
			}
			if !time.Now().Before(deadline) {
				t.Fatalf("codex route mode %q (version %q, %v), want %q; %s", mode, version, err, want, f.enrollmentEvidence("codex"))
			}
		}
		if want == "daemon" && (version != "0.171.0" || host != pid) {
			t.Fatalf("daemon launch version %q host %d, want the server's release and process %d", version, host, pid)
		}
		if want != "daemon" && host != 0 {
			t.Fatalf("a %s launch named a server %d", want, host)
		}
	}

	startUntil("startup", true, true, "daemon")
	if out := f.owner().mustPlain("owner", "who", "--all"); !strings.Contains(out, "publish: candidate (no sender for codex)") {
		t.Fatalf("a daemon session should wait only for a sender:\n%s", out)
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
