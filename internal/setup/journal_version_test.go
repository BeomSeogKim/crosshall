package setup

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/setuptest"
)

func TestCommandRuleJournalVersionTracksOwnership(t *testing.T) {
	f := setuptest.New(t, "codex", "agy")
	s := scopedService(f)
	ctx := t.Context()
	adapters := []string{"codex", "agy"}
	writeStatusJournal(t, f, emptyJournal())
	legacy, _, err := readJournal(f.State)
	if err != nil || legacy.Version != 1 || len(legacy.RuleFiles) != 0 {
		t.Fatalf("read legacy journal: %+v (%v)", legacy, err)
	}
	install := func() {
		t.Helper()
		plans, err := s.PlanInstall(ctx, adapters)
		if err != nil || len(plans) != len(adapters) {
			t.Fatalf("setup plans: %+v (%v)", plans, err)
		}
		for _, plan := range plans {
			f.CheckTargets(t)
			if err := s.Install(ctx, plan); err != nil {
				t.Fatal(err)
			}
		}
	}
	install()
	current, _, err := readJournal(f.State)
	if err != nil || current.Version != skillJournalVersion || len(current.RuleFiles) != len(adapters) ||
		len(current.SkillFiles) != len(adapters) {
		t.Fatalf("journal with command rule ownership: %+v (%v)", current, err)
	}

	current.Version = 1
	raw, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.State, journalName), raw, 0600); err != nil {
		t.Fatal(err)
	}
	install()
	migrated, _, err := readJournal(f.State)
	if err != nil || migrated.Version != skillJournalVersion || len(migrated.RuleFiles) != len(adapters) {
		t.Fatalf("setup did not upgrade the existing ownership record: %+v (%v)", migrated, err)
	}

	plans, err := s.PlanUninstall(ctx, adapters)
	if err != nil || len(plans) != len(adapters) {
		t.Fatalf("uninstall plans: %+v (%v)", plans, err)
	}
	for _, plan := range plans {
		f.CheckTargets(t)
		if err := s.Uninstall(ctx, plan); err != nil {
			t.Fatal(err)
		}
		record := migrated.RuleFiles[plan.Adapter]
		if _, err := os.Stat(record.Path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("uninstall left %s: %v", record.Path, err)
		}
	}
	cleaned, _, err := readJournal(f.State)
	if err != nil || cleaned.Version != 1 || len(cleaned.RuleFiles) != 0 || len(cleaned.SkillFiles) != 0 || cleaned.Pending != nil {
		t.Fatalf("journal after removing command rules: %+v (%v)", cleaned, err)
	}
}

func TestPendingCommandRulesUseVersionTwoBeforePublication(t *testing.T) {
	f := setuptest.New(t, "codex")
	j := emptyJournal()
	j.Pending = &operation{Action: "setup", Adapters: []string{"codex"}, Edits: []edit{{Adapter: "codex", Rules: true}}}
	writeStatusJournal(t, f, j)
	got, _, err := readJournal(f.State)
	if err != nil || got.Version != 2 || len(got.RuleFiles) != 0 || got.Pending == nil || !got.Pending.Edits[0].Rules {
		t.Fatalf("journal before publication: %+v (%v)", got, err)
	}
}

func TestPendingClassifierRulesUseVersionThreeBeforePublication(t *testing.T) {
	f := setuptest.New(t, "claude")
	j := emptyJournal()
	j.Pending = &operation{Action: "setup", Adapters: []string{"claude"}, Edits: []edit{{
		Adapter: "claude", Add: claudeAutoModeEntries(filepath.Join(f.Bin, "crosshall"), true),
	}}}
	writeStatusJournal(t, f, j)
	got, _, err := readJournal(f.State)
	if err != nil || got.Version != 3 || len(got.Files) != 0 || got.Pending == nil || !got.Pending.Edits[0].Add[0].AutoMode {
		t.Fatalf("classifier journal before publication: %+v (%v)", got, err)
	}
}
