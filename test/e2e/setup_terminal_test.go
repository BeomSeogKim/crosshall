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

func TestSetupAsksPerVendorInATerminal(t *testing.T) {
	f := newSetupWorld(t)
	codexBefore := readSetupFile(t, f.paths["codex"])
	// Claude yes with read rules only; Codex no.
	code, out := f.inTerminal(f.env, "y\ny\nn\nn\n", nil, "setup")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{
		"Install Claude Code hooks in " + f.paths["claude"] + " (from CLAUDE_CONFIG_DIR)? [y/N]",
		"Add Claude Code read-command allow rules? [y/N]",
		"Install Codex hooks in " + f.paths["codex"] + " (from CODEX_HOME)? [y/N]",
		"claude: installed", "codex: declined",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	claude := readSetupFile(t, f.paths["claude"]).bytes
	if !bytes.Contains(claude, []byte("Bash(crosshall read")) || bytes.Contains(claude, []byte("Bash(crosshall note")) {
		t.Fatalf("claude settings: %s", claude)
	}
	assertSetupFile(t, f.paths["codex"], codexBefore, true)
	offers, err := os.ReadFile(filepath.Join(f.state, "setup-offers.json"))
	if err != nil || !bytes.Contains(offers, []byte(`"codex":{"answer":"no"`)) || bytes.Contains(offers, []byte(`"claude":{"answer":"no"`)) {
		t.Fatalf("remembered offers %s: %v", offers, err)
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
