package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/hook"
	"github.com/BeomSeogKim/crosshall/internal/setuptest"
)

// handlerCommand reads the one handler command of an entry in either hooks
// file layout.
func handlerCommand(t *testing.T, entry Entry) string {
	t.Helper()
	var list struct{ Hooks []hookHandler }
	var group map[string][]hookHandler
	if entry.Group == "" {
		if err := json.Unmarshal(entry.Value, &list); err != nil || len(list.Hooks) != 1 {
			t.Fatalf("entry %s: %v", entry.Value, err)
		}
		return list.Hooks[0].Command
	}
	if err := json.Unmarshal(entry.Value, &group); err != nil || len(group[entry.Event]) != 1 {
		t.Fatalf("entry %s: %v", entry.Value, err)
	}
	return group[entry.Event][0].Command
}

// Each installer reads back the hook commands it writes, pinned or not, and
// those of earlier releases, and tells them from another executable's.
func TestInstallersReadTheirOwnHookCommands(t *testing.T) {
	const exe, old, root = "/opt/new/crosshall", "/opt/old/crosshall", "/state root"
	pin := hookStateAssignment + shellQuote(root) + " "
	for _, spec := range hook.InstallSpecs() {
		i := installerFor(spec.Adapter)
		entries, err := hookEntries(spec, exe, root)
		if err != nil || len(entries) == 0 {
			t.Fatalf("%s entries: %v", spec.Adapter, err)
		}
		for _, entry := range entries {
			command := handlerCommand(t, entry)
			if pinned := strings.HasPrefix(command, pin); pinned != i.statePinned() || (spec.Encoding != "shell" && pinned) {
				t.Errorf("%s %s pin %t: %q", spec.Adapter, entry.Event, pinned, command)
			}
			if spec.Encoding == "shell" && !i.recognizeShellCommand(command, entry.Event, spec) {
				t.Errorf("%s does not recognize its own %s hook %q", spec.Adapter, entry.Event, command)
			}
			if !i.checksRelocation() {
				continue
			}
			if got, err := i.recordedExecutable(entry, spec); err != nil || got != exe {
				t.Errorf("%s %s records executable %q (%v)", spec.Adapter, entry.Event, got, err)
			}
			if !i.runsExecutable(command, exe, spec) || i.runsExecutable(command, old, spec) {
				t.Errorf("%s %s runs-executable check wrong for %q", spec.Adapter, entry.Event, command)
			}
		}
	}
	codex := installerFor("codex")
	spec := hook.InstallSpec{Adapter: "codex", Encoding: "shell"}
	unpinned := shellQuote(exe) + " hook codex Stop"
	if !codex.recognizeShellCommand(unpinned, "Stop", spec) || !codex.runsExecutable(unpinned, exe, spec) {
		t.Errorf("codex does not read its earlier unpinned hook %q", unpinned)
	}
	if codex.recognizeShellCommand(pin+shellQuote(exe)+" hook codex Stop", "SessionStart", spec) {
		t.Error("codex recognized a hook for another event")
	}
	agy := installerFor("agy")
	spec = hook.InstallSpec{Adapter: "agy", Encoding: "shell"}
	if !agy.recognizeShellCommand(pin+shellQuote("/opt/renamed")+" hook agy SessionStart", "SessionStart", spec) {
		t.Error("agy does not recognize a pinned hook whose executable was renamed")
	}
	if agy.recognizeShellCommand(shellQuote("/opt/renamed")+" hook agy SessionStart", "SessionStart", spec) {
		t.Error("agy recognized an unpinned hook of another executable")
	}
}

// An Antigravity install for one executable is a crosshall group that a
// setup for another executable must not touch; the owner uninstalls first.
func TestAgySetupForAnotherExecutableLeavesTheGroup(t *testing.T) {
	f := setuptest.New(t, "agy")
	target := filepath.Join(f.Home, ".gemini", "config", "hooks.json")
	s := scopedService(f)
	plans, err := s.PlanInstall(t.Context(), []string{"agy"})
	plan := requireAgyPlan(t, plans, err)
	f.CheckTargets(t)
	if err := s.Install(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
	installed := configBytes(t, target)
	s.Executable = filepath.Join(f.Root, "relocated", "crosshall")
	if err := os.MkdirAll(filepath.Dir(s.Executable), 0700); err != nil {
		t.Fatal(err)
	}
	plans, err = s.PlanInstall(t.Context(), []string{"agy"})
	if err != nil || len(plans) != 1 || plans[0].Err == nil || !strings.Contains(plans[0].Err.Error(), "inspect it before setup") {
		t.Fatalf("expected the existing group to stop setup, got %+v (%v)", plans, err)
	}
	if got := configBytes(t, target); string(got) != string(installed) {
		t.Fatalf("setup for another executable changed the group: %s", got)
	}
}
