package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/hook"
	"github.com/BeomSeogKim/crosshall/internal/setuptest"
)

func TestCodexSetupPinsStateRootThroughShell(t *testing.T) {
	f := setuptest.New(t, "codex")
	service := scopedService(f)
	service.StateRoot = filepath.Join(f.Root, "alternate state's store")
	service.Executable = filepath.Join(f.Root, "program with a space's", "crosshall")
	if err := os.Mkdir(filepath.Dir(service.Executable), 0700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\nprintf '%s\\n' \"$CROSSHALL_STATE_DIR\" \"$@\"\n"
	if err := os.WriteFile(service.Executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}

	plan := codexInstallPlan(t, service)
	if plan.Err != nil {
		t.Fatal(plan.Err)
	}
	if _, err := os.Stat(plan.Path); !os.IsNotExist(err) {
		t.Fatalf("preview created hooks.json: %v", err)
	}
	f.CheckTargets(t)
	if err := service.Install(t.Context(), plan, ClaudeRules{}); err != nil {
		t.Fatal(err)
	}
	command := codexSessionStartCommand(t, configBytes(t, plan.Path))
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", command)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "CROSSHALL_STATE_DIR=/ignored-cold-root"}
	out, err := cmd.CombinedOutput()
	if want := service.StateRoot + "\nhook\ncodex\nSessionStart\n"; err != nil || string(out) != want {
		t.Fatalf("hook invocation produced %q, want %q (%v)", out, want, err)
	}
	if got := codexRoutingStatus(t, service.StateRoot).CodexHookStateRouting; got != "pinned" {
		t.Fatalf("routing status = %q, want pinned", got)
	}

	next := codexInstallPlan(t, service)
	if next.Err != nil || len(next.Files[0].Add) != 0 {
		t.Fatalf("same-root setup is not idempotent: %+v", next)
	}
}

func TestCodexSetupRefusesOneConfigForDifferentRoots(t *testing.T) {
	f := setuptest.New(t, "codex")
	service := scopedService(f)
	plan := codexInstallPlan(t, service)
	if plan.Err != nil {
		t.Fatal(plan.Err)
	}
	f.CheckTargets(t)
	if err := service.Install(t.Context(), plan, ClaudeRules{}); err != nil {
		t.Fatal(err)
	}
	first := configBytes(t, plan.Path)
	other := service
	other.StateRoot = filepath.Join(f.Root, "another-root")
	other.Executable = filepath.Join(f.Root, "other release", "crosshall")
	conflict := codexInstallPlan(t, other)
	if conflict.Err == nil || !strings.Contains(conflict.Err.Error(), "CODEX_HOME") {
		t.Fatalf("shared configuration accepted a distinct root: %v", conflict.Err)
	}
	if err := other.Install(t.Context(), conflict, ClaudeRules{}); err == nil {
		t.Fatal("applying a conflicted preview succeeded")
	}
	if got := configBytes(t, plan.Path); !bytes.Equal(got, first) {
		t.Fatalf("conflict changed hooks.json: %s", got)
	}
	if _, err := os.Stat(other.StateRoot); !os.IsNotExist(err) {
		t.Fatalf("conflict wrote alternate state: %v", err)
	}
	if got := codexRoutingStatus(t, other.StateRoot); got.CodexHookStateRouting != "pinned" || got.InstallState != "drifted" {
		t.Fatalf("different root inspection = %+v, want pinned drift", got)
	}

	t.Setenv("CODEX_HOME", filepath.Join(f.Home, "second codex"))
	independent := codexInstallPlan(t, other)
	f.CheckTargets(t)
	if independent.Err != nil || other.Install(t.Context(), independent, ClaudeRules{}) != nil {
		t.Fatalf("different config refused: %v", independent.Err)
	}
	if got := configBytes(t, plan.Path); !bytes.Equal(got, first) {
		t.Fatalf("install to separate config changed the first home: %s", got)
	}
}

func TestCodexSetupMigratesOwnedLegacyAndUninstallsExactly(t *testing.T) {
	f := setuptest.New(t, "codex")
	service := scopedService(f)
	original := []byte("{\n  \"unrelated\": true,\n  \"hooks\": {\"Stop\": [{\"hooks\": [{\"type\": \"command\", \"command\": \"/owner-tool\"}]}]}\n}\n")
	path, old := writeLegacyCodex(t, f, service, original, true)
	service.Executable = filepath.Join(f.Root, "upgraded crosshall")
	if got := codexRoutingStatus(t, service.StateRoot); got.CodexHookStateRouting != "inherited" || got.InstallState != "drifted" {
		t.Errorf("legacy inspection = %+v, want inherited drift", got)
	}
	plan := codexInstallPlan(t, service)
	if plan.Err != nil || len(plan.Files[0].Remove) == 0 || len(plan.Files[0].Add) != len(plan.Files[0].Remove) {
		t.Fatalf("setup needs a replacement preview: err=%v, files=%d", plan.Err, len(plan.Files))
	}
	setPolicy := service.SetPolicy
	interruption := errors.New("interrupted before enabling policy")
	service.SetPolicy = func(context.Context, string, bool) error { return interruption }
	f.CheckTargets(t)
	if err := service.Install(t.Context(), plan, ClaudeRules{}); !errors.Is(err, interruption) {
		t.Fatalf("expected an interruption after replacing the config: %v", err)
	}
	current := configBytes(t, path)
	command := codexSessionStartCommand(t, current)
	if bytes.Equal(old, current) || !strings.HasPrefix(command, "CROSSHALL_STATE_DIR=") ||
		!strings.Contains(command, shellQuote(service.Executable)) || !bytes.Contains(current, []byte("/owner-tool")) {
		t.Fatalf("incorrect migration: %s", current)
	}
	j, _, err := readJournal(service.StateRoot)
	if err != nil || j.Pending == nil || len(j.Pending.Edits[0].Restore) == 0 || len(j.Pending.Edits[0].Add) == 0 {
		t.Fatalf("pending migration must preserve its restore and add steps: %+v, %v", j, err)
	}
	service.SetPolicy = setPolicy
	resume := codexInstallPlan(t, service)
	if resume.Err != nil || !resume.Resume {
		t.Fatalf("migration recovery preview: resume=%t, err=%v", resume.Resume, resume.Err)
	}
	f.CheckTargets(t)
	if err := service.Install(t.Context(), resume, ClaudeRules{}); err != nil {
		t.Fatal(err)
	}
	j, _, err = readJournal(service.StateRoot)
	if err != nil || j.Pending != nil || len(j.Files["codex"].Entries) != len(plan.Files[0].Add) {
		t.Fatalf("migration journal: %+v, %v", j, err)
	}
	plans, err := service.PlanUninstall(t.Context(), []string{"codex"})
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
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0640 {
		t.Fatalf("uninstall did not preserve mode: %v, %v", info, err)
	}
}

func TestCodexSetupRetainsUnmanagedOrDriftedLegacy(t *testing.T) {
	for _, test := range []struct {
		name                   string
		owned, edited, badUndo bool
	}{
		{name: "unowned"},
		{name: "changed owned", owned: true, edited: true},
		{name: "unprovable undo", owned: true, badUndo: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := setuptest.New(t, "codex")
			service := scopedService(f)
			path, original := writeLegacyCodex(t, f, service, []byte("{\"unrelated\": true}\n"), test.owned)
			if test.edited {
				original = bytes.Replace(original, []byte(`"timeout":1`), []byte(`"timeout":2`), 1)
				f.CheckTargets(t)
				if err := os.WriteFile(path, original, 0640); err != nil {
					t.Fatal(err)
				}
			}
			if test.badUndo {
				j, _, err := readJournal(service.StateRoot)
				if err != nil {
					t.Fatal(err)
				}
				record := j.Files["codex"]
				record.Undo = [][]span{{{Start: 0, End: 1}}}
				j.Files["codex"] = record
				writeStatusJournal(t, f, j)
			}
			plan := codexInstallPlan(t, service)
			if plan.Err == nil || !strings.Contains(plan.Err.Error(), "CODEX_HOME") {
				t.Fatalf("setup accepted unsafe legacy configuration: %v", plan.Err)
			}
			if got := configBytes(t, path); !bytes.Equal(got, original) {
				t.Fatalf("setup preview changed legacy hooks: %s", got)
			}
		})
	}
}

func TestCodexSetupReportsMixedRoutingAndRefusesIt(t *testing.T) {
	f := setuptest.New(t, "codex")
	service := scopedService(f)
	path, old := writeLegacyCodex(t, f, service, []byte("{}\n"), false)
	specs, err := selectedSpecs([]string{"codex"})
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := hookEntries(specs[0], service.Executable, service.StateRoot)
	if err != nil {
		t.Fatal(err)
	}
	mixed, _, err := addEntries(old, pinned[:1])
	if err != nil {
		t.Fatal(err)
	}
	f.CheckTargets(t)
	if err := os.WriteFile(path, mixed, 0640); err != nil {
		t.Fatal(err)
	}
	if got := codexRoutingStatus(t, service.StateRoot); got.CodexHookStateRouting != "mixed" || got.InstallState != "drifted" {
		t.Fatalf("mixed configuration inspection = %+v", got)
	}
	if got := codexInstallPlan(t, service); got.Err == nil {
		t.Fatal("setup accepted mixed legacy and pinned routing")
	}
}

func codexInstallPlan(t *testing.T, service Service) VendorPlan {
	t.Helper()
	plans, err := service.PlanInstall(t.Context(), []string{"codex"})
	if err != nil || len(plans) != 1 {
		t.Fatalf("Codex preview: %+v, %v", plans, err)
	}
	return plans[0]
}

func codexSessionStartCommand(t *testing.T, body []byte) string {
	t.Helper()
	var data struct {
		Hooks map[string][]struct {
			Hooks []struct{ Command string }
		}
	}
	if err := json.Unmarshal(body, &data); err != nil {
		t.Fatal(err)
	}
	groups := data.Hooks["SessionStart"]
	if len(groups) != 1 || len(groups[0].Hooks) != 1 {
		t.Fatalf("expected one SessionStart hook: %s", body)
	}
	return groups[0].Hooks[0].Command
}

func writeLegacyCodex(t *testing.T, f setuptest.Fixture, service Service, original []byte, owned bool) (string, []byte) {
	t.Helper()
	var spec hook.InstallSpec
	for _, vendor := range hook.InstallSpecs() {
		if vendor.Adapter == "codex" {
			spec = vendor
		}
	}
	target, err := resolveConfigPath(spec)
	if err != nil {
		t.Fatal(err)
	}
	f.CheckTargets(t)
	if err := os.Mkdir(filepath.Dir(target.Path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target.Path, original, 0640); err != nil {
		t.Fatal(err)
	}
	start, err := readConfig(target.Path)
	if err != nil {
		t.Fatal(err)
	}
	var legacy []Entry
	for _, event := range spec.Events {
		group := map[string]any{"hooks": []map[string]any{
			{"type": "command", "command": shellQuote(service.Executable) + " hook codex " + event, "timeout": spec.Timeout},
		}}
		if matcher := spec.Matchers[event]; matcher != "" {
			group["matcher"] = matcher
		}
		encoded, err := json.Marshal(group)
		if err != nil {
			t.Fatal(err)
		}
		legacy = append(legacy, Entry{Event: event, Value: encoded})
	}
	body, added, err := addEntries(original, legacy)
	if err != nil {
		t.Fatal(err)
	}
	f.CheckTargets(t)
	if err := replaceConfig(target.Path, start, body, start.Mode, false); err != nil {
		t.Fatal(err)
	}
	current, err := readConfig(target.Path)
	if err != nil {
		t.Fatal(err)
	}
	if owned {
		undo, err := insertionSpans(original, body)
		if err != nil {
			t.Fatal(err)
		}
		j := emptyJournal()
		j.Files["codex"] = ownedFile{Path: target.Path, Original: stamp(start), Latest: stamp(current), Entries: added, Undo: [][]span{undo}}
		writeStatusJournal(t, f, j)
	}
	return target.Path, body
}

func codexRoutingStatus(t *testing.T, stateRoot string) InstallStatus {
	t.Helper()
	for _, status := range Inspect(stateRoot) {
		if status.Adapter == "codex" {
			return status
		}
	}
	t.Fatal("missing Codex diagnostic")
	return InstallStatus{}
}
