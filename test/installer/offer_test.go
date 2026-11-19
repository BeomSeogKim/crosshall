//go:build installer

package installer_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/setuptest"
)

// Exercise the installer up to its setup --offer call. The tagged recorder stands in for
// setup's own consent and footprint checks. install_test.sh builds real Go tags.
func TestInstallerOffer(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("installer terminal tests require python3: %v", err)
	}
	repo, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	f := setuptest.New(t, "claude", "codex")
	t.Setenv("PATH", f.Bin+":/usr/bin:/bin")
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(f.Home, "claude override"))
	for _, key := range []string{"GOFLAGS", "GOENV", "GOWORK", "GOTOOLCHAIN", "TEST_TELEMETRY_DIR"} {
		t.Setenv(key, "caller-value")
	}
	markers := []string{"CI", "CROSSHALL_ROUTE", "CLAUDE_CODE_SESSION_ID", "CODEX_THREAD_ID",
		"ANTIGRAVITY_CONVERSATION_ID", "CLAUDECODE", "AI_AGENT", "CODEX_SESSION_ID", "ANTIGRAVITY_AGENT"}
	for _, key := range markers {
		t.Setenv(key, "")
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	source := filepath.Join(f.Root, "source")
	if err := os.MkdirAll(filepath.Join(source, "scripts"), 0700); err != nil {
		t.Fatal(err)
	}
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0700); err != nil {
			t.Fatal(err)
		}
	}
	script := filepath.Join(source, "scripts", "install.sh")
	installer, err := os.ReadFile(filepath.Join(repo, "scripts", "install.sh"))
	if err != nil {
		t.Fatal(err)
	}
	write(script, string(installer))
	write(filepath.Join(source, "recorder"), `#!/bin/sh
case "$*" in
  version) echo v0.1.0; exit 0 ;;
  "setup --offer") ;;
  *) echo "unexpected arguments: $*" >&2; exit 2 ;;
esac
printf '%s\n' "$0" "$GOENV" "$GOFLAGS" "$GOWORK" "$GOTOOLCHAIN" "$TEST_TELEMETRY_DIR" \
  "$HOME" "$CROSSHALL_STATE_DIR" "$CLAUDE_CONFIG_DIR" "${CODEX_HOME-unset}" >> "$OFFER_LOG"
exit "$OFFER_EXIT"
`)
	write(filepath.Join(f.Bin, "crosshall"), "#!/bin/sh\necho PATH-decoy >> \"$OFFER_LOG\"\nexit 1\n")
	write(filepath.Join(f.Bin, "go"), `#!/bin/sh
set -eu
[ "$GOENV:$GOFLAGS:$GOTOOLCHAIN:$GOWORK" = off::local:off ]
case "$1" in
  env) echo "$HOME/empty-cache" ;;
  build)
    while [ "$1" != -o ]; do shift; done
    cp recorder "$2" && chmod 700 "$2" ;;
  version) printf '\tpath\tgithub.com/BeomSeogKim/crosshall/cmd/crosshall\n' ;;
  *) exit 2 ;;
esac
`)
	for _, args := range [][]string{{"init", "-q"}, {"add", "."},
		{"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "-qm", "fixture"},
		{"tag", "v0.1.0"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = source
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}

	dest := filepath.Join(f.Root, "install location")
	target := filepath.Join(dest, "crosshall")
	terminal := filepath.Join(repo, "test/setupfixture/terminal.py")
	check := func(t *testing.T, offered, failSetup bool, terminalArgs ...string) {
		t.Helper()
		dir, err := os.MkdirTemp(f.Root, "calls-")
		if err != nil {
			t.Fatal(err)
		}
		log := filepath.Join(dir, "offer")
		t.Setenv("OFFER_LOG", log)
		t.Setenv("OFFER_EXIT", "0")
		if failSetup {
			t.Setenv("OFFER_EXIT", "1")
		}
		f.CheckTargets(t)
		args := append([]string{"-B", terminal}, terminalArgs...)
		cmd := exec.Command(python, append(args, "--", script, "--source", dest)...)
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("binary installation failed: %v: %s", err, output)
		}
		lines := strings.Split(strings.TrimSpace(strings.ReplaceAll(string(output), "\r\n", "\n")), "\n")
		if len(lines) < 3 || lines[0] != "Installed crosshall v0.1.0 at "+target ||
			!strings.HasPrefix(lines[1], "export PATH=") || lines[2] != "Next (in a plain terminal): crosshall owner start" ||
			(!failSetup && len(lines) != 3) {
			t.Fatalf("unexpected install output: %q", output)
		}
		if failSetup && (!strings.Contains(string(output), "Optional adapter setup failed") || !strings.Contains(string(output), "setup to retry")) {
			t.Fatalf("no separate setup result and retry: %q", output)
		}
		if version, err := exec.Command(target, "version").Output(); err != nil || string(version) != "v0.1.0\n" {
			t.Fatalf("published executable: %q, %v", version, err)
		}
		seen, err := os.ReadFile(log)
		if !offered {
			if !os.IsNotExist(err) {
				t.Fatalf("unexpected setup invocation: %q, %v", seen, err)
			}
			return
		}
		want := strings.Join([]string{target, "caller-value", "caller-value", "caller-value", "caller-value", "caller-value",
			f.Home, f.State, os.Getenv("CLAUDE_CONFIG_DIR"), "unset", ""}, "\n")
		if err != nil || string(seen) != want {
			t.Fatalf("installed path or inherited environment: %q, %v; want %q", seen, err, want)
		}
	}

	t.Run("fresh owner terminal", func(t *testing.T) { check(t, true, false) })
	t.Run("upgrade reaches footprint check", func(t *testing.T) { check(t, true, false) })
	t.Run("optional setup failure keeps binary", func(t *testing.T) { check(t, true, true) })
	t.Run("piped input", func(t *testing.T) { check(t, false, false, "--pipe-in") })
	t.Run("piped output", func(t *testing.T) { check(t, false, false, "--pipe-out") })
	t.Run("both piped", func(t *testing.T) { check(t, false, false, "--pipe-in", "--pipe-out") })
	for _, marker := range markers {
		t.Run(marker, func(t *testing.T) {
			t.Setenv(marker, "") // Presence alone disqualifies the terminal.
			check(t, false, false)
		})
	}
}
