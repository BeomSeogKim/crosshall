package setup

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/setuptest"
)

// An install from a release that wrote Claude Code's bare read and send
// rules still sets up: those optional rules are not a previous scope.
func TestClaudeSetupKeepsEarlierOptionalRules(t *testing.T) {
	f := setuptest.New(t, "claude")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(f.Home, ".claude"))
	s := scopedService(f)
	ctx := t.Context()
	f.CheckTargets(t)
	j, digest, err := readJournal(s.StateRoot)
	if err != nil {
		t.Fatal(err)
	}
	targets, err := s.targets(ctx, j)
	if err != nil || len(targets) != 1 {
		t.Fatalf("targets: %v", err)
	}
	targets[0].Wanted = slices.DeleteFunc(targets[0].Wanted, func(e Entry) bool { return e.Event == "" })
	targets[0].Wanted = slices.Concat(targets[0].Wanted, claudeRules(false), claudeRules(true))
	op, err := makeInstall(j, targets)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.apply(ctx, digest, op); err != nil {
		t.Fatalf("earlier install: %v", err)
	}
	plans, err := s.PlanInstall(ctx, []string{"claude"})
	if err != nil || len(plans) != 1 || plans[0].Err != nil {
		t.Fatalf("setup over the earlier optional rules: %+v (%v)", plans, err)
	}
	f.CheckTargets(t)
	if err := s.Install(ctx, plans[0]); err != nil {
		t.Fatal(err)
	}
	settings := configBytes(t, filepath.Join(f.Home, ".claude", "settings.json"))
	if !bytes.Contains(settings, claudeRules(true)[0].Value) || !bytes.Contains(settings, claudeRules(false)[0].Value) {
		t.Fatalf("setup dropped the earlier optional rules: %s", settings)
	}
}

// A JSON rules file that exists but is empty is refused, not overwritten.
func TestAgySetupRefusesAnEmptyRulesFile(t *testing.T) {
	f := setuptest.New(t, "agy")
	rules := filepath.Join(f.Home, ".gemini", "antigravity-cli", "settings.json")
	f.CheckTargets(t)
	if err := os.MkdirAll(filepath.Dir(rules), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rules, nil, 0600); err != nil {
		t.Fatal(err)
	}
	plans, err := scopedService(f).PlanInstall(t.Context(), []string{"agy"})
	if err != nil || len(plans) != 1 || plans[0].Err == nil || !strings.Contains(plans[0].Err.Error(), "command rules configuration is empty") {
		t.Fatalf("expected the empty rules file to stop setup, got %+v (%v)", plans, err)
	}
	if got := configBytes(t, rules); len(got) != 0 {
		t.Fatalf("setup wrote the empty rules file: %s", got)
	}
}
