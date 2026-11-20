package setup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/setuptest"
)

func TestAgySetupOwnsOnlyItsNamedSessionStartGroup(t *testing.T) {
	const original = "{\n  \"owner-hook\": {\"SessionStart\": [ {\"command\": \"owner-command\"} ]}\n}\n"
	const withOwnerAddition = "{\n  \"owner-hook\": {\"SessionStart\": [ {\"command\": \"owner-command\"} ]},\"added\":{\"enabled\":false}\n}\n"
	for _, test := range []struct {
		name, before, after string
		editAfterInstall    bool
	}{
		{name: "fresh home"},
		{name: "existing user hooks", before: original, after: original},
		{name: "owner adds another group", before: original, after: withOwnerAddition, editAfterInstall: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := setuptest.New(t, "agy")
			target := filepath.Join(f.Home, ".gemini", "config", "hooks.json")
			executable := filepath.Join(f.Bin, "crosshall's fixture")
			program := "#!/bin/sh\nprintf '%s\\n' \"$CROSSHALL_STATE_DIR\" \"$@\"\n"
			if err := os.WriteFile(executable, []byte(program), 0700); err != nil {
				t.Fatal(err)
			}
			if test.before != "" {
				f.CheckTargets(t)
				if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, []byte(test.before), 0640); err != nil {
					t.Fatal(err)
				}
			}
			var policies []string
			s := Service{
				StateRoot: f.State, Executable: executable,
				SetPolicy: func(_ context.Context, adapter string, enabled bool) error {
					if enabled {
						policies = append(policies, adapter+" on")
					} else {
						policies = append(policies, adapter+" off")
					}
					return nil
				},
			}
			ctx := t.Context()
			selected := []string{"agy"}
			plans, err := s.PlanInstall(ctx, selected)
			plan := requireAgyPlan(t, plans, err)
			if plan.Path != target || plan.PathSource != "default" || plan.Name != "Antigravity" || len(plan.Approvals) != 0 {
				t.Fatalf("install preview = %+v", plan)
			}
			if test.before == "" {
				if _, err := os.Stat(filepath.Join(f.Home, ".gemini")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("preview wrote configuration: %v", err)
				}
			}
			f.CheckTargets(t)
			if err := s.Install(ctx, plan, ClaudeRules{}); err != nil {
				t.Fatal(err)
			}
			installed := configBytes(t, target)
			const existing = "\"owner-hook\": {\"SessionStart\": [ {\"command\": \"owner-command\"} ]}"
			if test.before != "" && !bytes.Contains(installed, []byte(existing)) {
				t.Fatalf("existing group changed: %s", installed)
			}
			var groups map[string]map[string][]struct {
				Type    string `json:"type"`
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			}
			if err := json.Unmarshal(installed, &groups); err != nil {
				t.Fatal(err)
			}
			start := groups["crosshall"]["SessionStart"]
			if len(groups["crosshall"]) != 1 || len(start) != 1 || start[0].Type != "command" || start[0].Timeout != 1 {
				t.Fatalf("native named SessionStart group = %+v", groups["crosshall"])
			}
			command := exec.CommandContext(ctx, "/bin/sh", "-c", start[0].Command)
			command.Env = []string{"CROSSHALL_STATE_DIR=/wrong-state"}
			if output, err := command.Output(); err != nil || string(output) != f.State+"\nhook\nagy\nSessionStart\n" {
				t.Fatalf("hook command gave %q (%v)", output, err)
			}
			status := readAdapterStatuses(t, f, executable)[2]
			if status.Adapter != "agy" || status.CLI != "found" || status.Install != "installed" || !status.Owned {
				t.Fatalf("installed status = %+v", status)
			}
			j, _, err := readJournal(f.State)
			if err != nil || len(j.Files["agy"].Entries) != 1 || j.Files["agy"].Entries[0].Group != "crosshall" {
				t.Fatalf("journal owns wrong entries: %+v (%v)", j.Files["agy"], err)
			}
			if raw := configBytes(t, filepath.Join(f.State, journalName)); bytes.Contains(raw, []byte("owner-command")) {
				t.Fatalf("journal stored unrelated content: %s", raw)
			}
			if test.editAfterInstall {
				offset := bytes.LastIndexByte(installed, '}')
				updated := spliceConfig(installed, offset-1, offset-1, []byte(",\"added\":{\"enabled\":false}"))
				if !json.Valid(updated) {
					t.Fatalf("bad edited fixture %s", updated)
				}
				f.CheckTargets(t)
				if err := os.WriteFile(target, updated, 0640); err != nil {
					t.Fatal(err)
				}
			}
			plans, err = s.PlanUninstall(ctx, selected)
			plan = requireAgyPlan(t, plans, err)
			f.CheckTargets(t)
			if err := s.Uninstall(ctx, plan); err != nil {
				t.Fatal(err)
			}
			if test.after == "" {
				if _, err := os.Stat(target); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("uninstall left its newly created file: %v", err)
				}
			} else if restored := configBytes(t, target); string(restored) != test.after {
				t.Fatalf("uninstall restored %q, want %q", restored, test.after)
			}
			status = readAdapterStatuses(t, f, executable)[2]
			if status.Install != "none" || status.Owned || !reflect.DeepEqual(policies, []string{"agy on", "agy off"}) {
				t.Fatalf("uninstalled status = %+v, policy calls %v", status, policies)
			}
		})
	}
}

func TestAgySetupRefusesConflictsAndLeavesThemOwnedByTheUser(t *testing.T) {
	for _, before := range []string{
		"{\"crosshall\":{\"SessionStart\":[{\"command\":\"owner-owned hook\"}]}}",
		"{\"owner-group\":{\"SessionStart\":[{\"command\":\"crosshall hook agy SessionStart\"}]}}",
	} {
		f := setuptest.New(t, "agy")
		target := filepath.Join(f.Home, ".gemini", "config", "hooks.json")
		f.CheckTargets(t)
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(before), 0600); err != nil {
			t.Fatal(err)
		}
		s := scopedService(f)
		plans, err := s.PlanInstall(t.Context(), []string{"agy"})
		if err != nil || len(plans) != 1 || plans[0].Err == nil || !strings.Contains(plans[0].Err.Error(), "inspect it before setup") {
			t.Fatalf("expected conflict, got %+v (%v)", plans, err)
		}
		status := readAdapterStatuses(t, f, s.Executable)[2]
		if status.Install != "drift" || status.Owned || status.Footprint != "present" {
			t.Fatalf("conflict status = %+v", status)
		}
		plans, err = s.PlanUninstall(t.Context(), []string{"agy"})
		plan := requireAgyPlan(t, plans, err)
		f.CheckTargets(t)
		if err := s.Uninstall(t.Context(), plan); err != nil {
			t.Fatal(err)
		}
		if got := configBytes(t, target); string(got) != before {
			t.Fatalf("unowned hooks changed: %s", got)
		}
	}
}

func TestAgySetupResumesAfterCreatingOnlyTheFirstDirectory(t *testing.T) {
	f := setuptest.New(t, "agy")
	s := scopedService(f)
	ctx := t.Context()
	selected := []string{"agy"}
	plans, err := s.PlanInstall(ctx, selected)
	plan := requireAgyPlan(t, plans, err)
	j := emptyJournal()
	j.Pending = &plan.op
	writeStatusJournal(t, f, j)
	f.CheckTargets(t)
	if err := os.Mkdir(filepath.Join(f.Home, ".gemini"), 0700); err != nil {
		t.Fatal(err)
	}
	plans, err = s.PlanInstall(ctx, selected)
	resume := requireAgyPlan(t, plans, err)
	if !resume.Resume {
		t.Fatalf("partial directory creation did not allow recovery: %+v", resume)
	}
	f.CheckTargets(t)
	if err := s.Install(ctx, resume, ClaudeRules{}); err != nil {
		t.Fatal(err)
	}
	if status := readAdapterStatuses(t, f, s.Executable)[2]; status.Install != "installed" || !status.Owned {
		t.Fatalf("recovered status = %+v", status)
	}
}

func requireAgyPlan(t *testing.T, plans []VendorPlan, err error) VendorPlan {
	t.Helper()
	if err != nil || len(plans) != 1 || plans[0].Adapter != "agy" || plans[0].Err != nil {
		t.Fatalf("Antigravity plan = %+v (%v)", plans, err)
	}
	return plans[0]
}
