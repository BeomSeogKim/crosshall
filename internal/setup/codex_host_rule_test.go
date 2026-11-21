package setup

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/setuptest"
)

func TestCodexHostRuleRecognizesDeclarations(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		claimed      bool
	}{
		{"known host", `host_executable(paths=["/owner/crosshall"], name='crosshall')`, true},
		{"positional", `host_executable("crosshall", ["/owner/crosshall"])`, true},
		{"other host", `host_executable(name='git', paths=["/bin/git"])`, false},
		{"other host first", "host_executable(name='git', paths=[\"/bin/git\"])\nhost_executable(name='crosshall', paths=[])", true},
		{"dynamic name", `host_executable(name="cross" + "hall", paths=[])`, true},
		{"aliased", `declare = host_executable`, true},
		{"unrelated text", "# host_executable(name='crosshall', paths=[])\njustification = \"host_executable(name='crosshall', paths=[])\"", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := mayDeclareCrosshallHost([]byte(tc.source)); got != tc.claimed {
				t.Errorf("host claim = %t, want %t for %q", got, tc.claimed, tc.source)
			}
		})
	}
}

func TestCodexSetupKeepsAnExistingHostRule(t *testing.T) {
	f := setuptest.New(t, "codex")
	s := scopedService(f)
	home := filepath.Join(f.Home, ".codex")
	ownerPath := filepath.Join(home, "rules", "default.rules")
	path := filepath.Join(home, "rules", "crosshall.rules")
	if err := os.MkdirAll(filepath.Dir(ownerPath), 0700); err != nil {
		t.Fatal(err)
	}
	original := []byte("host_executable(name=\"crosshall\", paths=[\"/owner/bin/crosshall\"])\n")
	writeConfigFixture(t, ownerPath, original, 0600)
	f.CheckTargets(t)
	plan := codexInstallPlan(t, s)
	var preview bytes.Buffer
	if err := RenderVendorPlan(&preview, plan); err != nil {
		t.Fatal(err)
	}
	if plan.Err != nil || len(plan.Files) != 1 || len(plan.Retained) != 1 || plan.Retained[0].Path != ownerPath ||
		!strings.Contains(preview.String(), ownerPath) || !strings.Contains(preview.String(), "skipped Codex command rules") {
		t.Fatalf("existing host rule preview: %+v\n%s", plan, preview.String())
	}
	if err := s.Install(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(configBytes(t, filepath.Join(home, "hooks.json")), []byte("SessionStart")) {
		t.Fatal("setup did not install Codex hooks")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("setup wrote command rules beside the existing host: %v", err)
	}
	if got := configBytes(t, ownerPath); !bytes.Equal(original, got) {
		t.Fatalf("setup changed the existing host rule: %q", got)
	}
}

func TestCodexSetupRechecksHostRulesAfterApproval(t *testing.T) {
	f := setuptest.New(t, "codex")
	s := scopedService(f)
	f.CheckTargets(t)
	plan := codexInstallPlan(t, s)
	if plan.Err != nil || len(plan.Files) != 2 {
		t.Fatalf("expected the hook and command rule plans: %+v", plan)
	}
	ownerPath := filepath.Join(filepath.Dir(plan.Files[1].Path), "new.rules")
	if err := os.MkdirAll(filepath.Dir(ownerPath), 0700); err != nil {
		t.Fatal(err)
	}
	writeConfigFixture(t, ownerPath, []byte("host_executable(name=\"crosshall\", paths=[])\n"), 0600)
	if err := s.Install(t.Context(), plan); err == nil || !strings.Contains(err.Error(), ownerPath) {
		t.Fatalf("expected the new rule to invalidate the plan: %v", err)
	}
	if _, err := os.Stat(plan.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalidated setup wrote hooks: %v", err)
	}
}
