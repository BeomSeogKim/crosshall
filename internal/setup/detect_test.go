package setup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/hook"
	"github.com/BeomSeogKim/crosshall/internal/setuptest"
)

func TestResolveConfigPathUsesDefaultOnlyForAnAbsentOverride(t *testing.T) {
	fixture := setuptest.New(t)
	home := fixture.Home
	for _, spec := range hook.InstallSpecs() {
		t.Run(spec.Adapter, func(t *testing.T) {
			if spec.Adapter == "agy" {
				if spec.HomeEnv != "" {
					t.Fatalf("Antigravity has an undocumented configuration override %q", spec.HomeEnv)
				}
				assertConfigTarget(t, spec, configTarget{
					Home: home, Path: filepath.Join(home, ".gemini", "config", "hooks.json"), Source: "default",
				})
				return
			}
			unsetSetupVariable(t, spec.HomeEnv)
			defaultPath := filepath.Join(home, "."+spec.Adapter, spec.File)
			assertConfigTarget(t, spec, configTarget{Home: home, Path: defaultPath, Source: "default"})

			override := filepath.Join(home, "chosen home")
			t.Setenv(spec.HomeEnv, override)
			assertConfigTarget(t, spec, configTarget{Home: home, Path: filepath.Join(override, spec.File), Source: "override"})
			for _, invalid := range []string{"", "relative", "/", override + "/..", override + "\ninvalid"} {
				t.Setenv(spec.HomeEnv, invalid)
				if target, err := resolveConfigPath(spec); err == nil || !strings.Contains(err.Error(), spec.HomeEnv) {
					t.Fatalf("override %q resolved as %+v: %v", invalid, target, err)
				}
			}
			unsetSetupVariable(t, spec.HomeEnv)
			unsetSetupVariable(t, "HOME")
			if target, err := resolveConfigPath(spec); err == nil || !strings.Contains(err.Error(), "HOME") {
				t.Fatalf("missing HOME resolved as %+v: %v", target, err)
			}
		})
	}
}

func TestSetupDefaultsStayInsideTheFixture(t *testing.T) {
	fixture := setuptest.New(t, "claude", "codex")
	service := defaultPathService(fixture)
	paths := []string{filepath.Join(fixture.Home, ".claude", "settings.json"), filepath.Join(fixture.Home, ".codex", "hooks.json")}
	preview, err := runSetupTest(t, service, "setup", "", true)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if !strings.Contains(preview, path) {
			t.Errorf("preview did not show the default %s", path)
		}
		if _, err := os.Lstat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("preview created vendor home %s: %v", path, err)
		}
	}
	fixture.CheckTargets(t)
	if _, err := runSetupTest(t, service, "setup", "y\nn\nn\n", false); err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if raw, err := os.ReadFile(path); err != nil || !bytes.Contains(raw, []byte("SessionStart")) {
			t.Fatalf("default file %s missing installed hooks: %v", path, err)
		}
	}
}

func TestSetupRefusesDefaultSymlinkToOutsideSentinel(t *testing.T) {
	fixture := setuptest.New(t, "claude")
	service := defaultPathService(fixture)
	if err := os.Symlink(filepath.Join(fixture.Outside, ".claude"), filepath.Join(fixture.Home, ".claude")); err != nil {
		t.Fatal(err)
	}
	probe := filepath.Join(fixture.Home, "vendor-probed")
	writeVersionProbe(t, "claude", "\n: > "+shellQuote(probe)+"\nprintf '2.1.327\\n'\n")
	// The fixture rejects an escaping write target. Exercise only preflight
	// with the symlink present, and let the fixture verify the sentinel.
	if _, err := runSetupTest(t, service, "setup", "", true); err == nil {
		t.Fatal("setup accepted a default configuration directory symlink")
	}
	for _, path := range []string{probe, service.StateRoot} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("setup went past the failed preflight: %s (%v)", path, err)
		}
	}
}

func TestSetupDoesNotMigrateAnOverrideToTheDefault(t *testing.T) {
	fixture := setuptest.New(t, "claude")
	service := defaultPathService(fixture)
	selected := filepath.Join(fixture.Home, "chosen home")
	override := filepath.Join(selected, "settings.json")
	t.Setenv("CLAUDE_CONFIG_DIR", selected)
	fixture.CheckTargets(t)
	if _, err := runSetupTest(t, service, "setup", "y\nn\nn\n", false); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(override)
	if err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(service.StateRoot, journalName)
	journalBefore, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	unsetSetupVariable(t, "CLAUDE_CONFIG_DIR")
	fixture.CheckTargets(t)
	if _, err := runSetupTest(t, service, "setup", "y\n", false); err == nil || !strings.Contains(err.Error(), "another configuration home") {
		t.Fatalf("setup accepted a changed home: %v", err)
	}
	if after, err := os.ReadFile(override); err != nil || !bytes.Equal(after, before) {
		t.Fatalf("recorded settings changed: %v", err)
	}
	if after, err := os.ReadFile(journalPath); err != nil || !bytes.Equal(after, journalBefore) {
		t.Fatalf("ownership journal changed: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(fixture.Home, ".claude")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("setup wrote to the new default: %v", err)
	}
}

func defaultPathService(fixture setuptest.Fixture) Service {
	return Service{
		StateRoot: fixture.State, Executable: filepath.Join(fixture.Bin, "crosshall"),
		SetPolicy: func(context.Context, string, bool) error { return nil },
	}
}

func assertConfigTarget(t *testing.T, spec hook.InstallSpec, want configTarget) {
	t.Helper()
	if got, err := resolveConfigPath(spec); err != nil || got != want {
		t.Fatalf("resolved target: %+v (%v), want %+v", got, err, want)
	}
}

func unsetSetupVariable(t *testing.T, key string) {
	t.Helper()
	t.Setenv(key, "")
	if err := os.Unsetenv(key); err != nil {
		t.Fatal(err)
	}
}
