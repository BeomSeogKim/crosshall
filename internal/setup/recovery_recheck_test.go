package setup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecoveredUninstallReportsUnknownOwnershipAfterAtomicSave(t *testing.T) {
	service, path := interruptSetupAfterReplace(t)
	edited, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	saved := path + ".editor"
	if err := os.WriteFile(saved, edited, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(saved, path); err != nil {
		t.Fatal(err)
	}
	apply, err := runSetupTest(t, service, "uninstall", "yes\nyes\n", false)
	if err != nil || !strings.Contains(apply, "retain") || !strings.Contains(apply, path) {
		t.Fatalf("uninstall must report the unproven publication: %q, %v", apply, err)
	}
	preview, err := runSetupTest(t, service, "uninstall", "", true)
	if err != nil || !strings.Contains(preview, "retain") || !strings.Contains(preview, path) {
		t.Fatalf("retention was not recorded for future plans: %q, %v", preview, err)
	}
	j, _, err := readJournal(service.StateRoot)
	if err != nil || len(j.Files) != 0 || j.Pending != nil {
		t.Fatalf("uninstall claimed an unproven publication: %+v, %v", j.Files, err)
	}
	if raw, err := os.ReadFile(path); err != nil || !bytes.Equal(raw, edited) {
		t.Fatalf("uninstall rewrote the owner save: %v", err)
	}
}

func TestInterruptedUninstallDisablesPoliciesWithMovedVendorHome(t *testing.T) {
	service, path := setupFixture(t)
	if _, err := runSetupTest(t, service, "setup", "yes\n", false); err != nil {
		t.Fatal(err)
	}
	installed := configBytes(t, path)
	rules, _, err := memberCommandRules("claude", service.Executable)
	if err != nil {
		t.Fatal(err)
	}
	edited, _, err := removeEntries(installed, rules[:1])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, edited, 0600); err != nil {
		t.Fatal(err)
	}
	lost := errors.New("interrupted policy update")
	service.SetPolicy = func(context.Context, string, bool) error { return lost }
	if _, err := runSetupTest(t, service, "setup", "yes\n", false); !errors.Is(err, lost) {
		t.Fatalf("expected the second setup to stop with a pending edit, got %v", err)
	}
	_, beforeDigest, err := readJournal(service.StateRoot)
	if err != nil {
		t.Fatal(err)
	}
	home := filepath.Dir(path)
	moved := home + "-moved"
	if err := os.Rename(home, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, home); err != nil {
		t.Fatal(err)
	}
	movedPath := filepath.Join(moved, filepath.Base(path))
	before, err := os.ReadFile(movedPath)
	if err != nil {
		t.Fatal(err)
	}
	var disabled []string
	service.SetPolicy = func(_ context.Context, adapter string, enabled bool) error {
		if enabled {
			t.Fatalf("uninstall enabled %s", adapter)
		}
		disabled = append(disabled, adapter)
		return nil
	}
	out, err := runSetupTest(t, service, "uninstall", "yes\n", false)
	if err != nil || strings.Join(disabled, ",") != "claude,codex,agy" ||
		!strings.Contains(out, "retain") || !strings.Contains(out, path) {
		t.Fatalf("uninstall should disable all adapter policies and report the retained path: %q, %v, %v", out, disabled, err)
	}
	to, err := os.Readlink(home)
	if err != nil || to != moved {
		t.Fatalf("uninstall changed the owner link: %q, %v", to, err)
	}
	if after, err := os.ReadFile(movedPath); err != nil || !bytes.Equal(before, after) {
		t.Fatalf("uninstall changed the moved config: %v", err)
	}
	j, digest, err := readJournal(service.StateRoot)
	if err != nil || j.Pending == nil || digest != beforeDigest {
		t.Fatalf("pending journal evidence was lost: %v", err)
	}
}
