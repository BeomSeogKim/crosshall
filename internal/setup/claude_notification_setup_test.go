package setup

import (
	"bytes"
	"maps"
	"os"
	"path/filepath"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/setuptest"
)

func TestClaudeSetupUpgradesOwnedNotificationMatcher(t *testing.T) {
	f, service, path, original := installClaude2(t)
	upgraded := upgradeClaudeNotification(t, service, path)
	if got := autoModeAllowed(t, upgraded); len(got) != 2 || got[0] != "$defaults" {
		t.Fatalf("upgraded classifier rules = %v", got)
	}

	plans, err := service.PlanInstall(t.Context(), []string{"claude"})
	if err != nil || len(plans) != 1 || plans[0].Err != nil {
		t.Fatalf("reinstall plan: %+v, %v", plans, err)
	}
	f.CheckTargets(t)
	if err := service.Install(t.Context(), plans[0]); err != nil {
		t.Fatal(err)
	}
	if got := configBytes(t, path); !bytes.Equal(got, upgraded) {
		t.Fatalf("reinstall changed settings: %s", got)
	}

	plans, err = service.PlanUninstall(t.Context(), []string{"claude"})
	if err != nil || len(plans) != 1 || plans[0].Err != nil {
		t.Fatalf("uninstall plan: %+v, %v", plans, err)
	}
	f.CheckTargets(t)
	if err := service.Uninstall(t.Context(), plans[0]); err != nil {
		t.Fatal(err)
	}
	if got := configBytes(t, path); !bytes.Equal(got, original) {
		t.Fatalf("uninstall did not restore original bytes: got %q, want %q", got, original)
	}
}

// After the owner edited the file, the original bytes are gone: the upgrade
// replaces only setup's own notification hook and keeps the owner's edit,
// and uninstall removes setup's entries without touching it.
func TestClaudeSetupUpgradesTheNotificationMatcherAfterAnOwnerEdit(t *testing.T) {
	f, service, path, _ := installClaude2(t)
	edited := bytes.Replace(configBytes(t, path), []byte(`"theme": "dark"`), []byte(`"theme": "light"`), 1)
	if err := os.WriteFile(path, edited, 0640); err != nil {
		t.Fatal(err)
	}
	upgraded := upgradeClaudeNotification(t, service, path)
	if !bytes.Contains(upgraded, []byte(`"theme": "light"`)) {
		t.Fatalf("the upgrade lost the owner's edit: %s", upgraded)
	}

	plans, err := service.PlanUninstall(t.Context(), []string{"claude"})
	if err != nil || len(plans) != 1 || plans[0].Err != nil {
		t.Fatalf("uninstall plan: %+v, %v", plans, err)
	}
	f.CheckTargets(t)
	if err := service.Uninstall(t.Context(), plans[0]); err != nil {
		t.Fatal(err)
	}
	left := configBytes(t, path)
	if bytes.Contains(left, []byte("permission_prompt")) || bytes.Contains(left, []byte(`"hook","claude"`)) ||
		!bytes.Contains(left, []byte(`"matcher":"auth_success"`)) || !bytes.Contains(left, []byte(`"theme": "light"`)) {
		t.Fatalf("after uninstall: %s", left)
	}
}

// upgradeClaudeNotification installs the current revision over claude-2
// and returns the settings: the notification hook matches both prompts,
// once, and the owner's own notification hook stays.
func upgradeClaudeNotification(t *testing.T, service Service, path string) []byte {
	t.Helper()
	plans, err := service.PlanInstall(t.Context(), []string{"claude"})
	if err != nil || len(plans) != 1 || plans[0].Err != nil {
		t.Fatalf("upgrade plan: %+v, %v", plans, err)
	}
	if err := service.Install(t.Context(), plans[0]); err != nil {
		t.Fatal(err)
	}
	upgraded := configBytes(t, path)
	if bytes.Count(upgraded, []byte(`"matcher":"permission_prompt|idle_prompt"`)) != 1 ||
		bytes.Contains(upgraded, []byte(`"matcher":"permission_prompt"`)) ||
		!bytes.Contains(upgraded, []byte(`"matcher":"auth_success"`)) {
		t.Fatalf("upgraded notification hooks = %s", upgraded)
	}
	return upgraded
}

// installClaude2 installs the claude-2 hooks, whose notification hook
// matched permission prompts only, beside an owner's notification hook.
func installClaude2(t *testing.T) (setuptest.Fixture, Service, string, []byte) {
	t.Helper()
	f := setuptest.New(t, "claude")
	service := scopedService(f)
	path := filepath.Join(f.Home, ".claude", "settings.json")
	if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	original := []byte("{\n  \"hooks\": {\"Notification\": [{\"matcher\":\"auth_success\",\"hooks\":[{\"type\":\"command\",\"command\":\"/owner-tool\"}]}]},\n  \"theme\": \"dark\"\n}\n")
	writeConfigFixture(t, path, original, 0640)

	adapters := []string{"claude"}
	specs, err := selectedSpecs(adapters)
	if err != nil || len(specs) != 1 {
		t.Fatalf("Claude install spec: %v", err)
	}
	spec := specs[0]
	spec.Revision = "claude-2"
	spec.Matchers = maps.Clone(spec.Matchers)
	spec.Matchers["Notification"] = "permission_prompt"
	j, digest, err := readJournal(f.State)
	if err != nil {
		t.Fatal(err)
	}
	target, err := service.preflightInstall(spec, j)
	if err != nil {
		t.Fatal(err)
	}
	oldInstall, err := makeInstall(j, []installTarget{target})
	if err != nil {
		t.Fatal(err)
	}
	f.CheckTargets(t)
	if err := service.apply(t.Context(), digest, oldInstall); err != nil {
		t.Fatal(err)
	}
	return f, service, path, original
}
