package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/spool"
)

func setupFixture(t *testing.T) (Service, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	vendor := filepath.Join(home, "claude home")
	bin := filepath.Join(home, "fake-bin")
	for _, dir := range []string{vendor, bin} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("CLAUDE_CONFIG_DIR", vendor)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex home"))
	t.Setenv("PATH", bin)
	if err := os.WriteFile(filepath.Join(bin, "claude"), []byte("#!/bin/sh\nprintf '2.1.327 (Claude Code)\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	return Service{
		StateRoot: filepath.Join(home, "state"), Executable: filepath.Join(home, "crosshall binary"),
		SetPolicy: func(context.Context, string, bool) error { return nil },
	}, filepath.Join(vendor, "settings.json")
}

func runSetupTest(t *testing.T, service Service, action, input string, dry bool) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := service.Run(t.Context(), Request{
		Action: action, DryRun: dry, Terminal: action == "uninstall",
		Input: strings.NewReader(input), Output: &out, Prompt: io.Discard,
	})
	return out.String(), err
}

func TestUninstallWithoutFootprintSkipsPlanAndMutation(t *testing.T) {
	s, f, policies := realSetup(t)
	f.CheckTargets(t)
	for _, adapters := range [][]string{nil, {"claude"}} {
		for _, dry := range []bool{false, true} {
			var output, prompts bytes.Buffer
			input := strings.NewReader("yes\n")
			req := Request{Action: "uninstall", Adapters: adapters, DryRun: dry, Terminal: true,
				Input: input, Output: &output, Prompt: &prompts}
			if err := s.Run(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			if got := output.String(); !strings.HasPrefix(got, "No crosshall adapter hooks are installed") ||
				!strings.Contains(got, "run crosshall owner doctor") ||
				!strings.Contains(got, "docs/operations.md#removing-crosshall in your release checkout") ||
				strings.Contains(got, "https://") || strings.Contains(got, "policy") || strings.Contains(got, "[y/N]") {
				t.Errorf("uninstall %v output: %s", adapters, got)
			}
			output.Reset()
			req.JSON = true
			if err := s.Run(t.Context(), req); err != nil {
				t.Fatal(err)
			}
			if prompts.Len() != 0 || input.Len() != len("yes\n") {
				t.Errorf("uninstall %v prompted or read input: %q", adapters, prompts.String())
			}
			var result map[string]any
			if err := json.Unmarshal(output.Bytes(), &result); err != nil || result["kind"] != "uninstall" ||
				result["ok"] != true || result["dry_run"] != dry || strings.Contains(output.String(), "docs/operations.md") {
				t.Errorf("uninstall %v JSON output: %s (%v)", adapters, output.String(), err)
			}
		}
	}
	if len(policies) != 0 {
		t.Errorf("uninstall wrote policies: %v", policies)
	}
	if _, err := os.Stat(f.State); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("uninstall created state: %v", err)
	}
}

func TestSetupResumesAfterFileWriteBeforePolicyConfirmation(t *testing.T) {
	service, path := setupFixture(t)
	original := []byte("{\n  \"env\": {\"PRIVATE_SETTING\": \"fixture-local-value\"},\n  \"theme\": \"dark\"\n}\n")
	if err := os.WriteFile(path, original, 0640); err != nil {
		t.Fatal(err)
	}
	unavailable := errors.New("dispatcher unavailable")
	confirmed := false
	service.SetPolicy = func(_ context.Context, adapter string, enabled bool) error {
		if enabled && adapter != "claude" {
			t.Fatalf("setup enabled an undetected adapter %q", adapter)
		}
		if confirmed {
			return nil
		}
		return unavailable
	}
	if _, err := runSetupTest(t, service, "setup", "yes\n", false); !errors.Is(err, unavailable) {
		t.Fatalf("expected policy failure, got %v", err)
	}
	j, _, err := readJournal(service.StateRoot)
	if err != nil || j.Pending == nil || j.Pending.Edits[0].After.File == "" {
		t.Fatalf("recorded staged identity: %+v, %v", j.Pending, err)
	}
	rawJournal, err := os.ReadFile(filepath.Join(service.StateRoot, journalName))
	if err != nil || bytes.Contains(rawJournal, []byte("fixture-local-value")) {
		t.Fatal("the journal retained unrelated configuration")
	}
	installed, err := readConfig(path)
	prefix := "Bash(" + commandRuleShellPath(service.Executable)
	if err != nil || installed.Mode != 0640 || !bytes.Contains(installed.Bytes, []byte(prefix+" read:*)")) ||
		!bytes.Contains(installed.Bytes, []byte(prefix+" note:*)")) || bytes.Contains(installed.Bytes, []byte(prefix+" run:*)")) {
		t.Fatalf("installed config does not reflect the approved rules: %s %v", installed.Bytes, err)
	}
	confirmed = true
	if out, err := runSetupTest(t, service, "setup", "", false); err != nil || !strings.Contains(out, "Recovering") {
		t.Fatalf("resuming after crash: %v\n%s", err, out)
	}
	final, err := readConfig(path)
	if err != nil || !sameConfigSnapshot(final, installed) {
		t.Fatalf("resume rewrote the already-installed file: %v", err)
	}
	out, err := runSetupTest(t, service, "uninstall", "yes\n", false)
	if err != nil {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	restored, err := readConfig(path)
	if err != nil || !bytes.Equal(restored.Bytes, original) || restored.Mode != 0640 {
		t.Fatalf("restored config %q with mode %o: %v", restored.Bytes, restored.Mode, err)
	}
}

func TestSetupRecoveryLeavesMatchingConfigWithoutRecordedIdentity(t *testing.T) {
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
	edit := op.Edits[0]
	data, err := edit.materialize(nil)
	if err != nil {
		t.Fatal(err)
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
	// The owner independently installs the same entries before setup wrote its stage.
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	out, err := runSetupTest(t, service, "setup", "yes\nyes\nno\nno\n", false)
	if err != nil || !strings.Contains(out, "Recover the interrupted") {
		t.Fatalf("unowned file did not require recovery: %s (%v)", out, err)
	}
	current, err := readConfig(path)
	if err != nil || !bytes.Equal(current.Bytes, data) {
		t.Fatal("setup modified the independently created file")
	}
	dir, err := spool.OpenServerRoot(service.StateRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	after, _, err := readJournalAt(dir)
	if err != nil || len(after.Files) != 0 {
		t.Fatal("the independently created file was claimed")
	}
}

func TestUninstallLeavesEditedEntryAndOtherConfig(t *testing.T) {
	service, path := setupFixture(t)
	if err := os.WriteFile(path, []byte(`{"theme":"dark","hooks":{"Stop":[{"hooks":[{"type":"command","command":"/other-hook"}]}]}}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runSetupTest(t, service, "setup", "yes\nno\nno\n", false); err != nil {
		t.Fatal(err)
	}
	in, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := bytes.Replace(in, []byte(`"timeout":1`), []byte(`"timeout":3`), 1)
	if err := os.WriteFile(path, edited, 0600); err != nil {
		t.Fatal(err)
	}
	out, err := runSetupTest(t, service, "uninstall", "yes\n", false)
	if err != nil || !strings.Contains(out, "keep edited") {
		t.Fatalf("uninstall: %v\n%s", err, out)
	}
	current, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(current, []byte(`"timeout":3`)) ||
		!bytes.Contains(current, []byte("/other-hook")) || !bytes.Contains(current, []byte(`"theme":"dark"`)) ||
		bytes.Count(current, []byte("crosshall binary")) != 1 {
		t.Fatalf("uninstall kept the wrong entries: %s (%v)", current, err)
	}
}

func TestReinstallWithoutHookEditsPreservesOwnerConfigOnUninstall(t *testing.T) {
	service, path := setupFixture(t)
	if _, err := runSetupTest(t, service, "setup", "yes\nno\nno\n", false); err != nil {
		t.Fatal(err)
	}
	initial, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := bytes.Replace(initial, []byte(`{`), []byte(`{"owner_setting":true,`), 1)
	if err := os.WriteFile(path, edited, 0600); err != nil {
		t.Fatal(err)
	}
	// A vendor upgrade refreshes the journal without adding any hook groups.
	vendor := filepath.Join(os.Getenv("PATH"), "claude")
	if err := os.WriteFile(vendor, []byte("#!/bin/sh\nprintf '2.123.456 (Claude Code)\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := runSetupTest(t, service, "setup", "yes\nno\nno\n", false); err != nil {
		t.Fatal(err)
	}
	if _, err := runSetupTest(t, service, "uninstall", "yes\n", false); err != nil {
		t.Fatal(err)
	}
	left, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(left, []byte(`"owner_setting":true`)) ||
		bytes.Contains(left, []byte("crosshall binary")) {
		t.Fatalf("uninstall should preserve the owner's additions and remove its hooks: %s (%v)", left, err)
	}
}

func TestSetupFailsClosedOnMissingHomeVariable(t *testing.T) {
	service, path := setupFixture(t)
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	home := os.Getenv("HOME")
	fallback := filepath.Join(home, ".claude")
	if err := os.Mkdir(fallback, 0700); err != nil {
		t.Fatal(err)
	}
	// A malformed fallback would produce the wrong error if setup read it.
	if err := os.WriteFile(filepath.Join(fallback, "settings.json"), []byte("sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runSetupTest(t, service, "setup", "yes\n", false); err == nil || !strings.Contains(err.Error(), "CLAUDE_CONFIG_DIR") {
		t.Fatalf("missing vendor environment did not fail closed: %v", err)
	}
	for _, p := range []string{path, service.StateRoot} {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("unexpected file %s: %v", p, err)
		}
	}
}

func TestSetupCreatesOnlyTheConfirmedConfigurationFile(t *testing.T) {
	service, _ := setupFixture(t)
	vendorHome := filepath.Join(os.Getenv("HOME"), "new claude home")
	t.Setenv("CLAUDE_CONFIG_DIR", vendorHome)
	path := filepath.Join(vendorHome, "settings.json")
	if _, err := runSetupTest(t, service, "setup", "yes\n", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(vendorHome); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dry run created the home: %v", err)
	}
	if _, err := runSetupTest(t, service, "setup", "yes\nno\nno\n", false); err != nil {
		t.Fatal(err)
	}
	config, err := readConfig(path)
	if err != nil || !config.Exists || config.Mode != 0600 {
		t.Fatalf("created config has mode %o, exists=%t: %v", config.Mode, config.Exists, err)
	}
	if _, err := runSetupTest(t, service, "uninstall", "yes\n", false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("uninstall left the setup-created file: %v", err)
	}
}
