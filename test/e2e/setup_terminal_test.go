//go:build e2e

package e2e

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// inTerminal runs the installed binary under the shared pseudo-terminal
// harness, with each stream a terminal unless piped.
func (f *setupWorld) inTerminal(env []string, answers string, pipe []string, args ...string) (int, string) {
	f.t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		f.t.Fatal("the terminal tests need python3: ", err)
	}
	harness, err := filepath.Abs("../setupfixture/terminal.py")
	if err != nil {
		f.t.Fatal(err)
	}
	argv := append([]string{"-B", harness, "--answers", answers}, pipe...)
	argv = append(append(argv, "--", f.executable), args...)
	cmd := exec.CommandContext(f.t.Context(), python, argv...)
	cmd.Dir, cmd.Env = f.home, env
	out, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); ok {
		return exit.ExitCode(), string(out)
	} else if err != nil {
		f.t.Fatal(err)
	}
	return 0, string(out)
}

func TestSetupAsksForOneConsentInATerminal(t *testing.T) {
	f := newSetupWorld(t)
	code, out := f.inTerminal(f.env, "y\n", nil, "setup")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{
		"Claude Code hooks in " + f.paths["claude"] + " (from CLAUDE_CONFIG_DIR)",
		"Codex hooks in " + f.paths["codex"] + " (from CODEX_HOME)",
		"Install the listed hooks? [y/N]",
		"claude: installed", "codex: installed",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Count(out, "[y/N]") != 1 {
		t.Fatalf("expected one consent:\n%s", out)
	}
	claude := readSetupFile(t, f.paths["claude"]).bytes
	if !bytes.Contains(claude, []byte(`"permissions": {"allow":["Bash(existing)"]}`)) ||
		bytes.Contains(claude, []byte("Bash(crosshall")) {
		t.Fatalf("claude settings: %s", claude)
	}
	if codex := readSetupFile(t, f.paths["codex"]).bytes; !bytes.Contains(codex, []byte("hook codex SessionStart")) {
		t.Fatalf("codex settings: %s", codex)
	}
	offers, err := os.ReadFile(filepath.Join(f.state, "setup-offers.json"))
	if err != nil || !bytes.Contains(offers, []byte(`"codex":{"answer":"yes"`)) || !bytes.Contains(offers, []byte(`"claude":{"answer":"yes"`)) {
		t.Fatalf("remembered offers %s: %v", offers, err)
	}
}

func TestInstallerOfferRecordsOnePathAndHookConsent(t *testing.T) {
	for _, tc := range []struct {
		name, input, receipt string
	}{
		{"accept", "y\n", "yes\n"},
		{"decline", "n\n", "no\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSetupWorld(t)
			before := readSetupFile(t, f.paths["claude"])
			receipt := filepath.Join(f.home, "path-consent")
			wrapper := filepath.Join(f.home, "offer")
			program := "#!/bin/sh\nexec 3>\"$CROSSHALL_TEST_PATH_RECEIPT\"\nexec \"$CROSSHALL_TEST_BINARY\" \"$@\"\n"
			if err := os.WriteFile(wrapper, []byte(program), 0700); err != nil {
				t.Fatal(err)
			}
			env := append(slices.Clone(f.env), "CROSSHALL_INSTALL_PATH_CONSENT=1", "CROSSHALL_TEST_PATH_RECEIPT="+receipt, "CROSSHALL_TEST_BINARY="+f.executable)
			f.executable = wrapper
			code, out := f.inTerminal(env, tc.input, nil, "setup", "--offer")
			if code != 0 || strings.Count(out, "[y/N]") != 1 || !strings.Contains(out, "Apply the shown PATH change and install the listed hooks? [y/N]") {
				t.Fatalf("offer exit %d:\n%s", code, out)
			}
			if data, err := os.ReadFile(receipt); err != nil || string(data) != tc.receipt {
				t.Fatalf("PATH consent receipt %q: %v", data, err)
			}
			if tc.receipt == "yes\n" {
				if !strings.Contains(out, "claude: installed") || !strings.Contains(out, "codex: installed") {
					t.Fatalf("accepted offer did not install hooks:\n%s", out)
				}
			} else {
				assertSetupFile(t, f.paths["claude"], before, true)
			}
		})
	}
}

func TestManualSetupDeclineKeepsTheInstallerOffer(t *testing.T) {
	f := newSetupWorld(t)
	before := readSetupFile(t, f.paths["claude"])
	code, out := f.inTerminal(f.env, "n\n", nil, "setup")
	if code != 0 || !strings.Contains(out, "claude: declined") || !strings.Contains(out, "codex: declined") {
		t.Fatalf("manual setup exit %d:\n%s", code, out)
	}
	assertSetupFile(t, f.paths["claude"], before, true)
	if _, err := os.Stat(f.state); !os.IsNotExist(err) {
		t.Fatalf("manual decline created setup state: %v", err)
	}
	if code, out := f.inTerminal(f.env, "\n", nil, "setup", "--offer"); code != 0 ||
		strings.Count(out, "[y/N]") != 1 || !strings.Contains(out, "Install the listed hooks?") {
		t.Fatalf("installer offer after manual decline exited %d:\n%s", code, out)
	}
}

func TestSetupNeedsBothStreamsToBeATerminal(t *testing.T) {
	f := newSetupWorld(t)
	before := readSetupFile(t, f.paths["claude"])
	for _, pipe := range [][]string{{"--pipe-in"}, {"--pipe-out"}} {
		code, out := f.inTerminal(f.env, "y\ny\ny\ny\n", pipe, "setup")
		if code != 2 || !strings.Contains(out, "crosshall setup claude codex --apply") {
			t.Errorf("%v: exit %d:\n%s", pipe, code, out)
		}
		// The installer's form is silent without a terminal.
		if code, out := f.inTerminal(f.env, "y\ny\ny\ny\n", pipe, "setup", "--offer"); code != 0 || out != "" {
			t.Errorf("%v offer: exit %d: %q", pipe, code, out)
		}
	}
	assertSetupFile(t, f.paths["claude"], before, true)
	if _, err := os.Stat(f.state); !os.IsNotExist(err) {
		t.Fatalf("a refused setup created state: %v", err)
	}
}

// An agent-marked terminal is refused before discovery, preferences, probes
// or writes; the installer's form stays silent there.
func TestAnAgentMarkedTerminalIsRefused(t *testing.T) {
	f := newSetupWorld(t)
	before := readSetupFile(t, f.paths["claude"])
	for _, marker := range []string{"CLAUDE_CODE_SESSION_ID=", "CODEX_THREAD_ID=synthetic", "AI_AGENT="} {
		env := append(slices.Clone(f.env), marker)
		if code, out := f.inTerminal(env, "y\ny\ny\ny\n", nil, "setup"); code == 0 || !strings.Contains(out, "owner") || strings.Contains(out, "Install") {
			t.Errorf("%s setup: exit %d:\n%s", marker, code, out)
		}
		if code, out := f.inTerminal(env, "y\ny\ny\ny\n", nil, "setup", "--offer"); code != 0 || out != "" {
			t.Errorf("%s offer: exit %d: %q", marker, code, out)
		}
	}
	assertSetupFile(t, f.paths["claude"], before, true)
	if _, err := os.Stat(f.state); !os.IsNotExist(err) {
		t.Fatalf("a refused setup created state: %v", err)
	}
}

// The installer's form stays silent when the state cannot be used, with or
// without a terminal.
func TestTheOfferIsSilentWithUnusableState(t *testing.T) {
	f := newSetupWorld(t)
	env := slices.DeleteFunc(slices.Clone(f.env), func(kv string) bool { return strings.HasPrefix(kv, "CROSSHALL_STATE_DIR=") })
	env = append(env, "CROSSHALL_STATE_DIR=relative")
	for _, pipe := range [][]string{nil, {"--pipe-in"}} {
		if code, out := f.inTerminal(env, "y\ny\ny\ny\n", pipe, "setup", "--offer"); code != 0 || out != "" {
			t.Errorf("%v: exit %d: %q", pipe, code, out)
		}
	}
}
