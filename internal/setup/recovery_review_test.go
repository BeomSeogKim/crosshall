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

func interruptSetupAfterReplace(t *testing.T) (Service, string) {
	t.Helper()
	service, path := setupFixture(t)
	interrupted := errors.New("policy publication interrupted")
	service.SetPolicy = func(context.Context, string, bool) error { return interrupted }
	if _, err := runSetupTest(t, service, "setup", "yes\nno\nno\n", false); !errors.Is(err, interrupted) {
		t.Fatalf("expected an interrupted setup, got %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := bytes.Replace(raw, []byte("{"), []byte(`{"owner_setting":true,`), 1)
	if err := os.WriteFile(path, edited, 0600); err != nil {
		t.Fatal(err)
	}
	service.SetPolicy = func(context.Context, string, bool) error { return nil }
	return service, path
}

func TestRecoverSetupAfterAnOwnerEdit(t *testing.T) {
	service, path := interruptSetupAfterReplace(t)
	before, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runSetupTest(t, service, "setup", "yes\nyes\nno\nno\n", false); err != nil {
		t.Fatal(err)
	}
	after, err := readConfig(path)
	if err != nil || !sameConfigSnapshot(before, after) {
		t.Fatalf("recovery rewrote the changed config: %v", err)
	}
	j, _, err := readJournal(service.StateRoot)
	if err != nil || j.Pending != nil || len(j.Files["claude"].Entries) == 0 {
		t.Fatalf("recovery failed to retain proven ownership without a pending operation: %v", err)
	}
}

func TestUninstallRecoversAnInterruptedSetup(t *testing.T) {
	service, path := interruptSetupAfterReplace(t)
	var disabled []string
	service.SetPolicy = func(_ context.Context, adapter string, enabled bool) error {
		if enabled {
			t.Fatalf("uninstall enabled %s while recovering", adapter)
		}
		disabled = append(disabled, adapter)
		return nil
	}
	if _, err := runSetupTest(t, service, "uninstall", "yes\nyes\n", false); err != nil {
		t.Fatal(err)
	}
	left, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(left, []byte(`"owner_setting":true`)) ||
		bytes.Contains(left, []byte("crosshall binary")) || strings.Join(disabled, ",") != "claude,codex,agy" {
		t.Fatalf("uninstall did not retain owner settings and disable all adapter policies: %q, %v, %v", left, disabled, err)
	}
}

func TestOversizePlanNeverEntersTheJournal(t *testing.T) {
	service, path := setupFixture(t)
	raw := []byte(`{"padding":"` + strings.Repeat("x", maxConfigBytes-len(`{"padding":""}`)) + `"}`)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runSetupTest(t, service, "setup", "yes\nno\nno\n", false); err == nil {
		t.Fatal("accepted an oversize replacement")
	}
	j, _, err := readJournal(service.StateRoot)
	if err != nil || j.Pending != nil {
		t.Fatalf("oversize plan wedged the journal: pending=%t, err=%v", j.Pending != nil, err)
	}
	if _, err := os.Lstat(service.StateRoot); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oversize plan created installation state: %v", err)
	}
}

func TestUninstallDisablesAllAdaptersAndRetainsAmbiguousConfiguration(t *testing.T) {
	service, claude := setupFixture(t)
	codex := filepath.Join(os.Getenv("CODEX_HOME"), "hooks.json")
	if err := os.WriteFile(filepath.Join(os.Getenv("PATH"), "codex"),
		[]byte("#!/bin/sh\nprintf 'codex-cli 0.170.0\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := runSetupTest(t, service, "setup", "yes\nno\nno\n", false); err != nil {
		t.Fatal(err)
	}
	ambiguous := []byte(`{"hooks":{},"hooks":{"unknown":[]}}`)
	if err := os.WriteFile(claude, ambiguous, 0600); err != nil {
		t.Fatal(err)
	}
	var disabled []string
	service.SetPolicy = func(_ context.Context, adapter string, enabled bool) error {
		if enabled {
			t.Fatalf("uninstall enabled %s", adapter)
		}
		if _, err := os.Lstat(codex); err != nil {
			t.Fatalf("removed the safe config before all adapter policies were disabled: %v", err)
		}
		disabled = append(disabled, adapter)
		return nil
	}
	if out, err := runSetupTest(t, service, "uninstall", "", true); err != nil || !strings.Contains(out, "retain") || len(disabled) != 0 {
		t.Fatalf("preview must report retained files without disabling policy: %q, %v, %v", out, disabled, err)
	}
	if out, err := runSetupTest(t, service, "uninstall", "yes\n", false); err != nil || !strings.Contains(out, "retain") {
		t.Fatalf("uninstall must retain the ambiguous file: %q, %v", out, err)
	}
	if strings.Join(disabled, ",") != "claude,codex,agy" {
		t.Fatalf("policies disabled: %v", disabled)
	}
	if raw, err := os.ReadFile(claude); err != nil || !bytes.Equal(raw, ambiguous) {
		t.Fatalf("uninstall changed the retained file: %q, %v", raw, err)
	}
	if _, err := os.Lstat(codex); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("uninstall did not remove the safe config: %v", err)
	}
}

func TestUninstallDisablesAllAdaptersWhenConfigChangesAfterPreview(t *testing.T) {
	service, path := setupFixture(t)
	if _, err := runSetupTest(t, service, "setup", "yes\nno\nno\n", false); err != nil {
		t.Fatal(err)
	}
	j, digest, err := readJournal(service.StateRoot)
	if err != nil {
		t.Fatal(err)
	}
	op := makeUninstall(j)
	ownerEdit := []byte(`{"owner_setting":true}`)
	if err := os.WriteFile(path, ownerEdit, 0600); err != nil {
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
	err = service.apply(t.Context(), digest, op)
	if err == nil || !strings.Contains(err.Error(), "configuration changed after planning") ||
		strings.Join(disabled, ",") != "claude,codex,agy" {
		t.Fatalf("policy disablement should precede the stale-plan refusal: %v, %v", disabled, err)
	}
	if raw, err := os.ReadFile(path); err != nil || !bytes.Equal(raw, ownerEdit) {
		t.Fatalf("uninstall changed the owner edit: %q, %v", raw, err)
	}
}

func TestResumeCleansJournalStageBeforeRestaging(t *testing.T) {
	t.Run("named stage", func(t *testing.T) { checkResumeCleansStage(t, true) })
	t.Run("legacy stage identity", func(t *testing.T) { checkResumeCleansStage(t, false) })
}

func checkResumeCleansStage(t *testing.T, named bool) {
	t.Helper()
	service, path := setupFixture(t)
	j := emptyJournal()
	targets, err := service.targets(t.Context(), j)
	if err != nil {
		t.Fatal(err)
	}
	op, err := makeInstall(j, targets)
	if err != nil {
		t.Fatal(err)
	}
	e := &op.Edits[0]
	cfgDir, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	defer cfgDir.Close()
	data, err := e.materialize(nil)
	if err != nil {
		t.Fatal(err)
	}
	stage, info, err := stageConfig(cfgDir, data, 0600)
	if err != nil {
		t.Fatal(err)
	}
	e.After.File = configIdentity(info)
	if named {
		e.Stage, e.StageDir = stage, e.Before.Dir
	}
	j.Pending = &op
	state, lock, err := service.lockState()
	if err != nil {
		t.Fatal(err)
	}
	if err := writeJournal(state, j); err != nil {
		t.Fatal(err)
	}
	lock.Close()
	state.Close()

	if _, err := runSetupTest(t, service, "setup", "", false); err != nil {
		t.Fatal(err)
	}
	if _, err := cfgDir.Lstat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retry left a copy of the config: %v", err)
	}
	if raw, err := os.ReadFile(path); err != nil || !bytes.Equal(raw, data) {
		t.Fatalf("retry did not install the journaled config: %v", err)
	}
}
