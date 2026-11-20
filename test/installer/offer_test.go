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
if [ "${CROSSHALL_INSTALL_PATH_CONSENT:-}" = 1 ] && [ "${OFFER_CONFIRM_PATH:-}" = 1 ]; then
  printf 'Apply the PATH and hook changes shown? [y/N] '
  read -r answer || answer=
  case $answer in
    y) printf 'yes\n' >&3 ;;
    *) printf 'no\n' >&3 ;;
  esac
fi
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
		start := "Next (in a plain terminal): " + strings.ReplaceAll(target, " ", `\ `) + " owner start"
		if len(lines) < 2 || lines[0] != "Installed crosshall v0.1.0 at "+target || lines[1] != start ||
			(!failSetup && len(lines) != 2) {
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

	for _, tc := range []struct {
		name, answer, prompt     string
		combined, piped, changed bool
		suffix, forge            bool
	}{
		{name: "path only", answer: "y\n", prompt: "Apply the PATH changes shown?", changed: true},
		{name: "shared yes", answer: "y\n", prompt: "Apply the PATH and hook changes shown?", combined: true, changed: true},
		{name: "shared no", answer: "n\n", prompt: "Apply the PATH and hook changes shown?", combined: true},
		{name: "piped", answer: "y\n", piped: true},
		{name: "suffix undo", answer: "y\n", prompt: "Apply the PATH changes shown?", changed: true, suffix: true},
		{name: "forged ownership", answer: "y\n", prompt: "Apply the PATH changes shown?", changed: true, forge: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := setuptest.New(t).Home
			installedDir := filepath.Join(home, ".local", "bin")
			t.Setenv("PATH", f.Bin+":"+installedDir+":/usr/bin:/bin")
			t.Setenv("BASH_ENV", "")
			zhome := filepath.Join(home, "zsh config")
			if err := os.Mkdir(zhome, 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("ZDOTDIR", zhome)
			t.Setenv("CROSSHALL_TEST_SHADOW_DIR", f.Bin)
			const reorder = "export PATH=\"$CROSSHALL_TEST_SHADOW_DIR:$HOME/.local/bin:$PATH\"\n"
			write(filepath.Join(zhome, ".zprofile"), reorder)
			write(filepath.Join(zhome, ".zshrc"), reorder)
			t.Setenv("OFFER_LOG", filepath.Join(home, "offer"))
			t.Setenv("OFFER_EXIT", "0")
			t.Setenv("OFFER_CONFIRM_PATH", "")
			if tc.combined {
				t.Setenv("OFFER_CONFIRM_PATH", "1")
			}
			login := filepath.Join(home, ".bash_profile")
			posix := filepath.Join(home, ".profile")
			bashrc := filepath.Join(home, ".bashrc")
			const existing = "export CROSSHALL_TEST_PROFILE=preserved\n"
			noFinalNewline := "export CROSSHALL_TEST_OTHER_PROFILE=preserved"
			if tc.suffix {
				noFinalNewline = "# An unterminated user comment"
			}
			write(login, existing)
			write(posix, noFinalNewline)
			if tc.forge {
				if err := os.WriteFile(bashrc, nil, 0600); err != nil {
					t.Fatal(err)
				}
			}
			run := func() string {
				args := []string{"-B", terminal, "--answers", tc.answer}
				if tc.piped {
					args = append(args, "--pipe-in")
				}
				cmd := exec.Command(python, append(args, "--", script, "--source")...)
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("install: %v\n%s", err, out)
				} else {
					return string(out)
				}
				return ""
			}
			out := run()
			prompts := strings.Count(out, "changes shown?")
			if tc.prompt == "" && prompts != 0 || tc.prompt != "" && (prompts != 1 || !strings.Contains(out, tc.prompt)) {
				t.Fatalf("unexpected confirmation: %s", out)
			}
			profile, err := os.ReadFile(login)
			if err != nil || !strings.HasPrefix(string(profile), existing) || (string(profile) != existing) != tc.changed {
				t.Fatalf("login profile: %q, %v", profile, err)
			}
			if !tc.changed {
				if got, err := os.ReadFile(posix); err != nil || string(got) != noFinalNewline {
					t.Fatalf("profile changed without consent: %q, %v", got, err)
				}
				for _, path := range []string{bashrc, filepath.Join(zhome, ".zshenv"), filepath.Join(zhome, ".zlogin")} {
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatalf("profile appeared without consent: %s (%v)", path, err)
					}
				}
				return
			}
			shown := strings.ReplaceAll(out, "\r\n", "\n")
			block := strings.TrimPrefix(string(profile), existing)
			if got := strings.Index(shown, block); got < 0 || got >= strings.Index(shown, tc.prompt) {
				t.Fatalf("written PATH block was not shown before confirmation:\n%s", shown)
			}
			shells := [][]string{{"bash", "-lc"}, {"bash", "-ic"}, {"sh", "-lc"}}
			if _, err := exec.LookPath("zsh"); err == nil {
				shells = append(shells, []string{"zsh", "-c"}, []string{"zsh", "-lc"}, []string{"zsh", "-ic"}, []string{"zsh", "-lic"})
			}
			for _, shell := range shells {
				cmd := exec.Command(shell[0], shell[1], "command -v crosshall && crosshall version")
				if version, err := cmd.Output(); err != nil || string(version) != filepath.Join(installedDir, "crosshall")+"\nv0.1.0\n" {
					t.Fatalf("new %v: %q, %v", shell, version, err)
				}
			}
			if out := run(); strings.Contains(out, "changes shown?") {
				t.Fatalf("reinstall asked about managed profiles: %s", out)
			}
			if again, err := os.ReadFile(login); err != nil || string(again) != string(profile) {
				t.Fatalf("reinstall changed the managed profile: %q, %v", again, err)
			}
			updatedBlock := strings.Replace(string(profile), "case ", "# owner edited the block\ncase ", 1)
			write(login, updatedBlock)
			if output, err := exec.Command(script, "--remove-path", login).CombinedOutput(); err == nil ||
				!strings.Contains(string(output), "Managed PATH block changed") {
				t.Fatalf("the edited block was not refused: %v\n%s", err, output)
			}
			if got, err := os.ReadFile(login); err != nil || string(got) != updatedBlock {
				t.Fatalf("removal changed the edited block: %q, %v", got, err)
			}
			write(login, string(profile))
			laterPath := login
			if tc.combined {
				laterPath = bashrc
			}
			if tc.suffix {
				laterPath = posix
			}
			const later = "export CROSSHALL_TEST_LATER=preserved"
			data, err := os.ReadFile(laterPath)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(laterPath, append(data, later...), 0600); err != nil {
				t.Fatal(err)
			}
			if tc.forge {
				profile, err := os.ReadFile(bashrc)
				if err != nil || !strings.Contains(string(profile), "# crosshall: user PATH existing\n") {
					t.Fatalf("existing empty profile: %q, %v", profile, err)
				}
				changedMarker := strings.Replace(string(profile), "# crosshall: user PATH existing\n", "# crosshall: user PATH created\n", 1)
				if err := os.WriteFile(bashrc, []byte(changedMarker), 0600); err != nil {
					t.Fatal(err)
				}
			}
			zshenv := filepath.Join(zhome, ".zshenv")
			zlogin := filepath.Join(zhome, ".zlogin")
			zshrc := filepath.Join(zhome, ".zshrc")
			if output, err := exec.Command(script, "--remove-path", login, posix, bashrc, zshenv, zlogin, zshrc).CombinedOutput(); err != nil {
				t.Fatalf("undo PATH: %v\n%s", err, output)
			}
			want := map[string]string{login: existing, posix: noFinalNewline, zshrc: reorder}
			if tc.suffix {
				want[posix] += "\n"
			}
			want[laterPath] += later
			if tc.forge {
				want[bashrc] = ""
			}
			for path, data := range want {
				if got, err := os.ReadFile(path); err != nil || string(got) != data {
					t.Errorf("undo %s: got %q, %v; want %q", path, got, err, data)
				}
			}
			for _, path := range []string{bashrc, zshenv, zlogin} {
				if _, keep := want[path]; keep {
					continue
				}
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Errorf("undo left an unchanged installer-created file: %s (%v)", path, err)
				}
			}
			if tc.suffix {
				cmd := exec.Command("/bin/sh", "-c", `. "$1"; printf '%s' "$CROSSHALL_TEST_LATER"`, "sh", posix)
				if got, err := cmd.Output(); err != nil || string(got) != "preserved" {
					t.Errorf("profile assignment after undo: %q, %v", got, err)
				}
			}
			if _, err := os.Stat(filepath.Join(installedDir, ".crosshall-path-owners")); !os.IsNotExist(err) {
				t.Errorf("undo left ownership records: %v", err)
			}
		})
	}
}
