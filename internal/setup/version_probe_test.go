package setup

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupVersionProbeUsesDisposableDirectories(t *testing.T) {
	service, settings := setupFixture(t)
	result := filepath.Join(t.TempDir(), "probe-result")
	home := os.Getenv("HOME")
	config := filepath.Dir(settings)
	t.Setenv("TMPDIR", config)
	t.Setenv("CROSSHALL_PROBE_PRIVATE", "inherited")
	writeVersionProbe(t, "claude", `
if [ "$1" != --version ]; then exit 1; fi
printf '%s\n' "$HOME" "$CLAUDE_CONFIG_DIR" "$PWD" "$TMPDIR" "${CROSSHALL_PROBE_PRIVATE:-unset}" > `+shellQuote(result)+`
: > "$HOME/probe-home"
: > "$CLAUDE_CONFIG_DIR/probe-config"
: > ./probe-cwd
if [ -n "$TMPDIR" ]; then : > "$TMPDIR/probe-temp"; fi
printf '2.3.4 (Claude Code)\n'
`)
	plan, err := runSetupTest(t, service, "setup", "", true)
	if err != nil || !strings.Contains(plan, "2.3.4") {
		t.Fatalf("preview did not report the probed version: %v\n%s", err, plan)
	}
	for _, marker := range []string{
		filepath.Join(home, "probe-home"), filepath.Join(home, "probe-cwd"),
		filepath.Join(config, "probe-config"), filepath.Join(config, "probe-temp"),
	} {
		if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("preview wrote outside the disposable probe: %s: %v", marker, err)
		}
	}
	raw, err := os.ReadFile(result)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(parts) != 5 || parts[4] != "unset" {
		t.Fatalf("version probe inherited unexpected environment: %q", raw)
	}
	for _, dir := range parts[:4] {
		if dir == "" {
			t.Error("version probe received an empty directory")
			continue
		}
		if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("probe directory was not disposable: %s: %v", dir, err)
		}
	}
}

func TestSetupPreflightsAllConfigsBeforeProbingVersions(t *testing.T) {
	for _, tc := range []struct {
		name string
		bad  func(*testing.T)
	}{
		{name: "missing later config variable", bad: func(t *testing.T) {
			t.Setenv("CODEX_HOME", "")
		}},
		{name: "malformed later config", bad: func(t *testing.T) {
			dir := os.Getenv("CODEX_HOME")
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "hooks.json"), []byte(`{"hooks":{},"hooks":{}}`), 0600); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service, _ := setupFixture(t)
			marker := filepath.Join(t.TempDir(), "earlier-vendor-ran")
			writeVersionProbe(t, "claude", "\n: > "+shellQuote(marker)+"\nprintf '2.3.4\\n'\n")
			writeVersionProbe(t, "codex", "\nprintf '2.3.4\\n'\n")
			tc.bad(t)
			if _, err := runSetupTest(t, service, "setup", "", true); err == nil {
				t.Fatal("preview accepted an invalid configuration")
			}
			if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("preview ran Claude before rejecting the Codex configuration: %v", err)
			}
		})
	}
}

func writeVersionProbe(t *testing.T, vendor, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(os.Getenv("PATH"), vendor), []byte("#!/bin/sh"+body), 0700); err != nil {
		t.Fatal(err)
	}
}
