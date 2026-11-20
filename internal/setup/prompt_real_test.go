package setup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/setuptest"
)

// The prompt layer over the real operations, in the shared fixture: vendor
// defaults under the fixture HOME, fake vendor CLIs, sentinels outside.
func realSetup(t *testing.T) (Service, setuptest.Fixture, map[string]bool) {
	t.Helper()
	f := setuptest.New(t, "claude", "codex")
	policies := map[string]bool{}
	s := Service{StateRoot: f.State, Executable: filepath.Join(f.Root, "crosshall"),
		SetPolicy: func(_ context.Context, adapter string, on bool) error { policies[adapter] = on; return nil }}
	return s, f, policies
}

func realRun(t *testing.T, s Service, f setuptest.Fixture, req Request, input string) (string, error) {
	t.Helper()
	f.CheckTargets(t)
	var out bytes.Buffer
	req.Output, req.Prompt, req.Input = &out, &out, strings.NewReader(input)
	if req.Action == "" {
		req.Action = "setup"
	}
	err := s.prompts(context.Background(), req, s)
	return out.String(), err
}

func TestTheTerminalFlowInstallsBaseHooksWithOneConsent(t *testing.T) {
	s, f, policies := realSetup(t)
	claude := filepath.Join(f.Home, ".claude", "settings.json")
	codex := filepath.Join(f.Home, ".codex", "hooks.json")
	if err := os.Mkdir(filepath.Dir(claude), 0700); err != nil {
		t.Fatal(err)
	}
	const approvals = `"permissions": {"allow":["Bash(existing)"], "deny":["Bash(blocked)"]}`
	if err := os.WriteFile(claude, []byte("{"+approvals+"}"), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := realRun(t, s, f, Request{Terminal: true}, "y\n")
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Count(out, "[y/N]") != 1 || !strings.Contains(out, "Claude Code hooks in "+claude+" (default under HOME)") ||
		!strings.Contains(out, "Codex hooks in "+codex+" (default under HOME)") {
		t.Fatalf("questions:\n%s", out)
	}
	settings, err := os.ReadFile(claude)
	if err != nil || !bytes.Contains(settings, []byte("SessionStart")) || !bytes.Contains(settings, []byte(approvals)) ||
		bytes.Contains(settings, []byte("Bash(crosshall")) {
		t.Fatalf("claude settings %s: %v", settings, err)
	}
	if settings, err := os.ReadFile(codex); err != nil || !bytes.Contains(settings, []byte("SessionStart")) {
		t.Fatalf("codex settings %s: %v", settings, err)
	}
	if !policies["claude"] || !policies["codex"] {
		t.Fatalf("policies %v", policies)
	}
	if d, err := s.declinedOffers(); err != nil || d["codex"] || d["claude"] {
		t.Fatalf("declined %v, %v", d, err)
	}
}

func TestTheInstallerOfferOverTheRealStatus(t *testing.T) {
	s, f, _ := realSetup(t)
	offer := Request{Offer: true, Terminal: true}
	out, err := realRun(t, s, f, offer, "\n")
	if err != nil || strings.Count(out, "[y/N]") != 1 || !strings.Contains(out, "Claude Code hooks in") || !strings.Contains(out, "Codex hooks in") {
		t.Fatalf("first offer: %v\n%s", err, out)
	}
	if _, err := os.Lstat(filepath.Join(f.Home, ".claude")); !os.IsNotExist(err) {
		t.Fatalf("a skipped offer wrote settings: %v", err)
	}
	if err := s.rememberAnswers(map[string]answer{"codex": answerNo}, snap(t, s), time.UnixMilli(1)); err != nil {
		t.Fatal(err)
	}
	out, err = realRun(t, s, f, offer, "y\n")
	if err != nil || strings.Contains(out, "Codex hooks in") || !strings.Contains(out, "claude: installed") {
		t.Fatalf("second offer: %v\n%s", err, out)
	}
	// Claude now has a footprint, so the installer asks nothing at all.
	if out, err = realRun(t, s, f, offer, "y\ny\ny\n"); err != nil || out != "" {
		t.Fatalf("offer after an install: %v\n%q", err, out)
	}
}

func TestAnInvalidOverrideStopsOnlyThatVendor(t *testing.T) {
	s, f, _ := realSetup(t)
	// The fixture proves the Claude target; the Codex override is invalid
	// and must fail before any Codex path is touched.
	f.CheckTargets(t)
	t.Setenv("CODEX_HOME", "relative/codex")
	var buf bytes.Buffer
	err := s.prompts(context.Background(), Request{Action: "setup", Terminal: true, Output: &buf, Prompt: &buf,
		Input: strings.NewReader("y\n")}, s)
	out := buf.String()
	if err == nil || !strings.Contains(out, "claude: installed") || !strings.Contains(out, "codex: failed") ||
		strings.Contains(out, "Codex hooks in") {
		t.Fatalf("%v\n%s", err, out)
	}
}

// While another setup holds the lock, an install fails for that vendor with
// its retry command and changes nothing.
func TestASecondSetupAtOnceFailsWithItsRetry(t *testing.T) {
	s, f, _ := realSetup(t)
	f.CheckTargets(t)
	err := s.WithSetupLock(func(*os.Root) error {
		out, err := realRun(t, s, f, Request{Apply: true, Adapters: []string{"claude"}}, "")
		if err == nil || !strings.Contains(out, "claude: failed") || !strings.Contains(out, "next: crosshall setup claude") {
			t.Fatalf("%v\n%s", err, out)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(f.Home, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("a locked-out setup wrote settings: %v", err)
	}
}

// A setup interrupted at its policy update, then an uninstall of the same
// vendor: the recovery question names the interrupted setup.
func TestAnUninstallAfterAnInterruptedSetupNamesTheSetup(t *testing.T) {
	s, f, _ := realSetup(t)
	s.SetPolicy = func(context.Context, string, bool) error { return errors.New("dispatcher unavailable") }
	if out, err := realRun(t, s, f, Request{Apply: true, Adapters: []string{"claude"}}, ""); err == nil {
		t.Fatalf("the interrupted setup succeeded:\n%s", out)
	}
	s.SetPolicy = func(context.Context, string, bool) error { return nil }
	out, err := realRun(t, s, f, Request{Action: "uninstall", Adapters: []string{"claude"}}, "n\n")
	if err != nil || !strings.Contains(out, "Recover the interrupted setup of claude without overwriting configuration?") {
		t.Fatalf("%v\n%s", err, out)
	}
}
