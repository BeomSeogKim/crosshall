package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/setuptest"
)

// installAgy1 installs the agy-1 hooks, SessionStart only, beside an
// owner's own hook group.
func installAgy1(t *testing.T) (setuptest.Fixture, Service, string, []byte) {
	t.Helper()
	f := setuptest.New(t, "agy")
	service := scopedService(f)
	path := filepath.Join(f.Home, ".gemini", "config", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	original := []byte("{\n  \"owner\": {\"PreInvocation\": [{\"type\":\"command\",\"command\":\"/owner-tool\"}]}\n}\n")
	writeConfigFixture(t, path, original, 0640)
	specs, err := selectedSpecs([]string{"agy"})
	if err != nil || len(specs) != 1 {
		t.Fatalf("Antigravity install spec: %v", err)
	}
	spec := specs[0]
	spec.Revision, spec.Events = "agy-1", []string{"SessionStart"}
	j, digest, err := readJournal(f.State)
	if err != nil {
		t.Fatal(err)
	}
	target, err := service.preflightInstall(spec, j)
	if err != nil {
		t.Fatal(err)
	}
	old, err := makeInstall(j, []installTarget{target})
	if err != nil {
		t.Fatal(err)
	}
	f.CheckTargets(t)
	if err := service.apply(t.Context(), digest, old); err != nil {
		t.Fatal(err)
	}
	return f, service, path, original
}

func agyInstallStatus(t *testing.T, service Service) InstallStatus {
	t.Helper()
	statuses := inspect(service.StateRoot, service.Executable)
	i := slices.IndexFunc(statuses, func(s InstallStatus) bool { return s.Adapter == "agy" })
	if i < 0 {
		t.Fatalf("no Antigravity status in %+v", statuses)
	}
	return statuses[i]
}

func crosshallGroupEvents(t *testing.T, path string) []string {
	t.Helper()
	var groups map[string]map[string]json.RawMessage
	if err := json.Unmarshal(configBytes(t, path), &groups); err != nil {
		t.Fatal(err)
	}
	if _, ok := groups["owner"]["PreInvocation"]; !ok {
		t.Fatalf("the owner's group changed: %v", groups)
	}
	var events []string
	for event := range groups["crosshall"] {
		events = append(events, event)
	}
	slices.Sort(events)
	return events
}

// Setup replaces the agy-1 group with agy-2's in one journaled edit, and
// uninstall leaves the owner's file as it was.
func TestAgySetupUpgradesTheAgy1Group(t *testing.T) {
	f, service, path, original := installAgy1(t)
	if got := crosshallGroupEvents(t, path); !slices.Equal(got, []string{"SessionStart"}) {
		t.Fatalf("agy-1 group = %v", got)
	}
	status := agyInstallStatus(t, service)
	if status.InstalledRev != "agy-1" || status.OutdatedHooks() == "" {
		t.Fatalf("agy-1 install not named outdated: %+v", status)
	}
	plans, err := service.PlanInstall(t.Context(), []string{"agy"})
	plan := requireAgyPlan(t, plans, err)
	f.CheckTargets(t)
	if err := service.Install(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
	if got := crosshallGroupEvents(t, path); !slices.Equal(got, []string{"PreInvocation", "SessionStart", "Stop"}) {
		t.Fatalf("agy-2 group = %v", got)
	}
	status = agyInstallStatus(t, service)
	if status.InstalledRev != "agy-2" || status.OutdatedHooks() != "" || status.InstallState != "installed" {
		t.Fatalf("after the upgrade: %+v", status)
	}
	plans, err = service.PlanUninstall(t.Context(), []string{"agy"})
	f.CheckTargets(t)
	if err := service.Uninstall(t.Context(), requireAgyPlan(t, plans, err)); err != nil {
		t.Fatal(err)
	}
	if got := configBytes(t, path); string(got) != string(original) {
		t.Fatalf("uninstall left %s, want %s", got, original)
	}
}

// After an owner edit, the upgrade removes only the agy-1 group.
func TestAgySetupUpgradesTheAgy1GroupAfterAnOwnerEdit(t *testing.T) {
	f, service, path, _ := installAgy1(t)
	edited := []byte(string(configBytes(t, path)[:1]) + "\n  \"later\": {\"Stop\": [{\"type\":\"command\",\"command\":\"/later\"}]}," + string(configBytes(t, path)[1:]))
	if err := os.WriteFile(path, edited, 0640); err != nil {
		t.Fatal(err)
	}
	plans, err := service.PlanInstall(t.Context(), []string{"agy"})
	plan := requireAgyPlan(t, plans, err)
	f.CheckTargets(t)
	if err := service.Install(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
	if got := crosshallGroupEvents(t, path); !slices.Equal(got, []string{"PreInvocation", "SessionStart", "Stop"}) {
		t.Fatalf("agy-2 group = %v", got)
	}
	var groups map[string]json.RawMessage
	if err := json.Unmarshal(configBytes(t, path), &groups); err != nil || groups["later"] == nil {
		t.Fatalf("the owner's later group is gone: %s (%v)", configBytes(t, path), err)
	}
	if status := agyInstallStatus(t, service); status.InstalledRev != "agy-2" || status.InstallState != "installed" {
		t.Fatalf("after the upgrade: %+v", status)
	}
}
