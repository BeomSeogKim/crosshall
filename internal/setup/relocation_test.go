package setup

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupRequiresUninstallBeforeExecutableRelocation(t *testing.T) {
	for _, adapter := range []string{"claude", "codex"} {
		t.Run(adapter, func(t *testing.T) {
			service, path := setupFixture(t)
			if adapter == "codex" {
				bin := filepath.Join(t.TempDir(), "fake-bin")
				if err := os.Mkdir(bin, 0700); err != nil {
					t.Fatal(err)
				}
				t.Setenv("PATH", bin)
				if err := os.WriteFile(filepath.Join(bin, "codex"), []byte("#!/bin/sh\nprintf 'codex-cli 0.1.0\\n'\n"), 0700); err != nil {
					t.Fatal(err)
				}
				path = filepath.Join(os.Getenv("CODEX_HOME"), "hooks.json")
			}
			if _, err := runSetupTest(t, service, "setup", "yes\nno\nno\n", false); err != nil {
				t.Fatal(err)
			}
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			service.Executable = filepath.Join(os.Getenv("HOME"), "new crosshall binary")
			if _, err := runSetupTest(t, service, "setup", "yes\nno\nno\n", false); err == nil || !strings.Contains(err.Error(), "crosshall uninstall") {
				t.Fatalf("relocation must ask for uninstall, got %v", err)
			}
			unchanged, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(unchanged, original) {
				t.Fatalf("refused setup changed vendor settings: %v", err)
			}
			if _, err := runSetupTest(t, service, "uninstall", "yes\n", false); err != nil {
				t.Fatal(err)
			}
			if _, err := runSetupTest(t, service, "setup", "yes\nno\nno\n", false); err != nil {
				t.Fatal(err)
			}
			relocated, err := os.ReadFile(path)
			if err != nil || !bytes.Contains(relocated, []byte(service.Executable)) || bytes.Contains(relocated, []byte(filepath.Join(os.Getenv("HOME"), "crosshall binary"))) {
				t.Fatalf("relocation after uninstall left the wrong hook commands: %s (%v)", relocated, err)
			}
		})
	}
}

func TestSetupRelocationRetainsOwnerEditedHook(t *testing.T) {
	service, path := setupFixture(t)
	if _, err := runSetupTest(t, service, "setup", "yes\nno\nno\n", false); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := bytes.Replace(original, []byte(`"timeout":1`), []byte(`"timeout":3`), 1)
	if err := os.WriteFile(path, edited, 0600); err != nil {
		t.Fatal(err)
	}
	service.Executable = filepath.Join(os.Getenv("HOME"), "new crosshall binary")
	if out, err := runSetupTest(t, service, "uninstall", "yes\n", false); err != nil || !strings.Contains(out, "keep edited") {
		t.Fatalf("uninstall did not report the edited hook: %v\n%s", err, out)
	}
	if _, err := runSetupTest(t, service, "setup", "yes\nno\nno\n", false); err == nil || !strings.Contains(err.Error(), "crosshall uninstall") {
		t.Fatalf("relocation must refuse while the owner-edited hook remains, got %v", err)
	}
	unchanged, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(unchanged, []byte(`"timeout":3`)) || bytes.Contains(unchanged, []byte(service.Executable)) {
		t.Fatalf("refused setup changed the edited hook: %s (%v)", unchanged, err)
	}
	if err := os.WriteFile(path, []byte(`{"owner_preference":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runSetupTest(t, service, "setup", "yes\nno\nno\n", false); err != nil {
		t.Fatalf("setup after the owner resolved the retained hook: %v", err)
	}
	j, _, err := readJournal(service.StateRoot)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range j.Files["claude"].Entries {
		if bytes.Contains(entry.Value, []byte(filepath.Join(os.Getenv("HOME"), "crosshall binary"))) {
			t.Fatalf("the journal still owns the retired hook: %s", entry.Value)
		}
	}
}
