package setup

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/setuptest"
)

// interruptVendorSetup applies a vendor's setup up to its policy
// publication, which fails, and leaves the operation pending.
func interruptVendorSetup(t *testing.T, f setuptest.Fixture, s Service, adapter string) {
	t.Helper()
	lost := errors.New("interrupted policy publication")
	s.SetPolicy = func(context.Context, string, bool) error { return lost }
	plans, err := s.PlanInstall(t.Context(), []string{adapter})
	if err != nil || len(plans) != 1 || plans[0].Err != nil {
		t.Fatalf("%s setup plan: %+v (%v)", adapter, plans, err)
	}
	f.CheckTargets(t)
	if err := s.Install(t.Context(), plans[0]); !errors.Is(err, lost) {
		t.Fatalf("expected an interrupted setup: %v", err)
	}
}

func TestSkillRecoveryDoesNotAdoptAnInPlaceOwnerEdit(t *testing.T) {
	f := setuptest.New(t, "claude")
	s := scopedService(f)
	path := filepath.Join(f.Home, ".claude", "skills", "crosshall", "SKILL.md")
	interruptVendorSetup(t, f, s, "claude")
	before, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	edited := append(bytes.Clone(skillContent), "\nOwner note.\n"...)
	f.CheckTargets(t)
	if err := os.WriteFile(path, edited, 0644); err != nil {
		t.Fatal(err)
	}
	if after, err := os.Lstat(path); err != nil || !os.SameFile(before, after) {
		t.Fatalf("fixture edit must keep the inode: %v", err)
	}

	recovery, err := s.PlanInstall(t.Context(), []string{"claude"})
	if err != nil || len(recovery) != 1 || !recovery[0].Recovery {
		t.Fatalf("expected a recovery plan: %+v (%v)", recovery, err)
	}
	f.CheckTargets(t)
	if err := s.Install(t.Context(), recovery[0]); err != nil {
		t.Fatal(err)
	}
	if j, _, err := readJournal(f.State); err != nil || len(j.SkillFiles) != 0 || len(j.Files) != 1 {
		t.Fatalf("recovery adopted the edited skill: %+v (%v)", j.SkillFiles, err)
	}
	installVendor(t, f, s, "claude")
	if got := configBytes(t, path); !bytes.Equal(got, edited) {
		t.Fatalf("setup after recovery replaced the edited skill: %q", got)
	}
	uninstallVendor(t, f, s, "claude")
	if got := configBytes(t, path); !bytes.Equal(got, edited) {
		t.Fatalf("uninstall after recovery removed the edited skill: %q", got)
	}
}

func TestSkillRecreatedIndependentlyIsNotClaimed(t *testing.T) {
	for _, action := range []string{"setup", "uninstall"} {
		t.Run(action, func(t *testing.T) {
			f := setuptest.New(t, "claude")
			s := scopedService(f)
			path := filepath.Join(f.Home, ".claude", "skills", "crosshall", "SKILL.md")
			installVendor(t, f, s, "claude")
			dir := filepath.Dir(path)
			f.CheckTargets(t)
			if err := os.Rename(dir, dir+".aside"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			writeConfigFixture(t, path, skillContent, 0644)
			replacement, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}

			var plan VendorPlan
			if action == "setup" {
				plan = installVendor(t, f, s, "claude")
			} else {
				plan = uninstallVendor(t, f, s, "claude")
			}
			if len(plan.Retained) != 1 || plan.Retained[0].Path != path {
				t.Fatalf("%s did not say it kept the recreated skill: %+v", action, plan.Retained)
			}
			if j, _, err := readJournal(f.State); err != nil || len(j.SkillFiles) != 0 {
				t.Fatalf("%s kept a claim on the recreated skill: %+v (%v)", action, j.SkillFiles, err)
			}
			uninstallVendor(t, f, s, "claude")
			current, err := os.Lstat(path)
			if err != nil || !os.SameFile(current, replacement) || !bytes.Equal(configBytes(t, path), skillContent) {
				t.Fatalf("the recreated skill was removed or replaced: %v", err)
			}
		})
	}
}

func TestSkillSharedByTwoVendorHomesStaysWhileEitherIsInstalled(t *testing.T) {
	for _, order := range [][]string{{"claude", "codex"}, {"codex", "claude"}} {
		t.Run(strings.Join(order, " then "), func(t *testing.T) {
			f := setuptest.New(t, "claude", "codex")
			s := scopedService(f)
			shared := filepath.Join(f.Root, "shared home")
			t.Setenv("CLAUDE_CONFIG_DIR", shared)
			t.Setenv("CODEX_HOME", shared)
			path := filepath.Join(shared, "skills", "crosshall", "SKILL.md")
			for _, adapter := range order {
				installVendor(t, f, s, adapter)
			}
			if j, _, err := readJournal(f.State); err != nil || len(j.SkillFiles) != 1 || j.SkillFiles[order[0]].Path != path {
				t.Fatalf("shared skill claims: %+v (%v)", j.SkillFiles, err)
			}

			removal := uninstallVendor(t, f, s, "claude")
			if !bytes.Equal(configBytes(t, path), skillContent) {
				t.Fatal("uninstalling Claude Code removed the skill Codex still uses")
			}
			if order[0] == "claude" && (len(removal.Retained) != 1 || !strings.Contains(removal.Retained[0].Reason, "Codex still uses it")) {
				t.Fatalf("uninstall did not say why it kept the skill: %+v", removal.Retained)
			}
			if j, _, err := readJournal(f.State); err != nil || len(j.SkillFiles) != 1 || j.SkillFiles["codex"].Path != path {
				t.Fatalf("the skill claim did not stay with Codex: %+v (%v)", j.SkillFiles, err)
			}
			uninstallVendor(t, f, s, "codex")
			if _, err := os.Lstat(filepath.Join(shared, "skills")); !os.IsNotExist(err) {
				t.Fatalf("the last uninstall left the skill: %v", err)
			}
		})
	}
}

func TestUnprovenUpgradeKeepsTheEarlierProvenClaims(t *testing.T) {
	f := setuptest.New(t, "claude")
	s := scopedService(f)
	settings := filepath.Join(f.Home, ".claude", "settings.json")
	skill := filepath.Join(f.Home, ".claude", "skills", "crosshall", "SKILL.md")
	installVendor(t, f, s, "claude")

	// An upgrade that edits the hooks file and then writes the skill.
	rules, _, err := memberCommandRules("claude", s.Executable)
	if err != nil {
		t.Fatal(err)
	}
	reduced, _, _, err := removeEntries(configBytes(t, settings), rules[:1])
	if err != nil {
		t.Fatal(err)
	}
	f.CheckTargets(t)
	if err := os.WriteFile(settings, reduced, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(skill); err != nil {
		t.Fatal(err)
	}
	interruptVendorSetup(t, f, s, "claude")

	// The owner's editor saves the hooks file atomically.
	saved := bytes.Replace(configBytes(t, settings), []byte("{"), []byte(`{"owner_setting":true,`), 1)
	f.CheckTargets(t)
	if err := os.WriteFile(settings+".editor", saved, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(settings+".editor", settings); err != nil {
		t.Fatal(err)
	}
	recovery, err := s.PlanUninstall(t.Context(), []string{"claude"})
	if err != nil || len(recovery) != 1 || !recovery[0].Recovery {
		t.Fatalf("expected a recovery plan: %+v (%v)", recovery, err)
	}
	f.CheckTargets(t)
	if err := s.Uninstall(t.Context(), recovery[0]); err != nil {
		t.Fatal(err)
	}
	uninstallVendor(t, f, s, "claude")
	after := configBytes(t, settings)
	if !bytes.Contains(after, []byte(`"owner_setting":true`)) {
		t.Fatalf("uninstall lost the owner's save: %s", after)
	}
	if bytes.Contains(after, []byte("hook")) || bytes.Contains(after, []byte("crosshall")) {
		t.Fatalf("uninstall left the earlier proven hooks and rules: %s", after)
	}
}

// sharedSkillHome points both Claude Code and Codex at one directory and
// installs them in order.
func sharedSkillHome(t *testing.T, order ...string) (setuptest.Fixture, Service, string, string) {
	t.Helper()
	f := setuptest.New(t, "claude", "codex")
	s := scopedService(f)
	shared := filepath.Join(f.Root, "shared home")
	if err := os.Mkdir(shared, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", shared)
	t.Setenv("CODEX_HOME", shared)
	for _, adapter := range order {
		installVendor(t, f, s, adapter)
	}
	return f, s, shared, filepath.Join(shared, "skills", "crosshall", "SKILL.md")
}

func TestScopedUninstallKeepsASharedSkillPerVendorConsent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		order  []string
		answer string
		kept   bool
	}{
		{"Claude first, yes then no", []string{"claude", "codex"}, "y\nn\n", true},
		{"Claude first, yes to both", []string{"claude", "codex"}, "y\ny\n", false},
		{"Codex first, yes then no", []string{"codex", "claude"}, "y\nn\n", true},
		{"Codex first, yes to both", []string{"codex", "claude"}, "y\ny\n", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, s, shared, path := sharedSkillHome(t)
			before := homeTree(t, shared)
			for _, adapter := range tc.order {
				installVendor(t, f, s, adapter)
			}
			out, err := realRun(t, s, f, Request{Action: "uninstall", Terminal: true, Adapters: []string{"claude", "codex"}}, tc.answer)
			if err != nil {
				t.Fatalf("uninstall: %v\n%s", err, out)
			}
			j, _, err := readJournal(f.State)
			if err != nil {
				t.Fatal(err)
			}
			if tc.kept {
				if !bytes.Equal(configBytes(t, path), skillContent) || j.SkillFiles["codex"].Path != path || len(j.SkillFiles) != 1 {
					t.Fatalf("Codex stays installed but lost the skill or its claim: %+v\n%s", j.SkillFiles, out)
				}
				return
			}
			if after := homeTree(t, shared); !maps.Equal(before, after) || len(j.SkillFiles) != 0 {
				t.Fatalf("uninstalling both was not byte-exact:\nbefore %v\nafter  %v\n%s", before, after, out)
			}
		})
	}
}

func TestSharedSkillFollowsTheRecordedCodexHome(t *testing.T) {
	f, s, _, path := sharedSkillHome(t, "claude", "codex")
	t.Setenv("CODEX_HOME", filepath.Join(f.Root, "another codex home"))
	removal := uninstallVendor(t, f, s, "claude")
	if !bytes.Equal(configBytes(t, path), skillContent) || len(removal.Retained) != 1 {
		t.Fatalf("uninstall from another CODEX_HOME removed the skill Codex uses: %+v", removal.Retained)
	}
	if j, _, err := readJournal(f.State); err != nil || j.SkillFiles["codex"].Path != path {
		t.Fatalf("the skill claim did not pass to Codex: %+v (%v)", j.SkillFiles, err)
	}
}

func TestSkillRemovalPreviewIsStaleOnceAnotherVendorShares(t *testing.T) {
	f, s, _, path := sharedSkillHome(t, "claude")
	plans, err := s.PlanUninstall(t.Context(), []string{"claude"})
	if err != nil || len(plans) != 1 || plans[0].Err != nil || vendorSkill(t, plans[0]).Skill != "remove" {
		t.Fatalf("Claude Code removal preview: %+v (%v)", plans, err)
	}
	installVendor(t, f, s, "codex")
	f.CheckTargets(t)
	if err := s.Uninstall(t.Context(), plans[0]); err == nil || !strings.Contains(err.Error(), "rerun") {
		t.Fatalf("a stale removal was accepted: %v", err)
	}
	if !bytes.Equal(configBytes(t, path), skillContent) {
		t.Fatal("the stale removal deleted the skill Codex now uses")
	}
}

func TestGroupedSetupInstallsASharedSkillOnce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		req    Request
		answer string
	}{
		{"one yes", Request{Terminal: true, Adapters: []string{"claude", "codex"}}, "y\n"},
		{"apply", Request{Apply: true, Adapters: []string{"claude", "codex"}}, ""},
		{"installer offer", Request{Offer: true, Terminal: true}, "y\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, s, shared, path := sharedSkillHome(t)
			before := homeTree(t, shared)
			out, err := realRun(t, s, f, tc.req, tc.answer)
			if err != nil {
				t.Fatalf("grouped setup: %v\n%s", err, out)
			}
			if !strings.Contains(out, "crosshall skill shared with Claude Code ("+path+")") {
				t.Errorf("plan does not show the shared skill:\n%s", out)
			}
			j, _, err := readJournal(f.State)
			if err != nil || len(j.Files) != 2 || len(j.SkillFiles) != 1 || j.SkillFiles["claude"].Path != path {
				t.Fatalf("grouped setup records: files %v, skills %+v (%v)\n%s", j.Files, j.SkillFiles, err, out)
			}
			if !bytes.Contains(configBytes(t, filepath.Join(shared, "hooks.json")), []byte("hook codex SessionStart")) ||
				!bytes.Contains(configBytes(t, filepath.Join(shared, "settings.json")), []byte("SessionStart")) ||
				!bytes.Equal(configBytes(t, path), skillContent) {
				t.Fatal("grouped setup did not install both vendors and the skill")
			}

			out, err = realRun(t, s, f, Request{Action: "uninstall", Apply: true, Adapters: []string{"claude", "codex"}}, "")
			if err != nil {
				t.Fatalf("uninstall: %v\n%s", err, out)
			}
			if after := homeTree(t, shared); !maps.Equal(before, after) {
				t.Fatalf("uninstall was not byte-exact:\nbefore %v\nafter  %v\n%s", before, after, out)
			}
		})
	}
}

func TestGroupedSetupStillRefusesAnExternalChange(t *testing.T) {
	f, s, shared, _ := sharedSkillHome(t)
	plans, err := s.PlanInstall(t.Context(), []string{"claude", "codex"})
	if err != nil || len(plans) != 2 || plans[0].Err != nil || plans[1].Err != nil {
		t.Fatalf("grouped plans: %+v (%v)", plans, err)
	}
	f.CheckTargets(t)
	if err := s.Install(t.Context(), plans[0]); err != nil {
		t.Fatal(err)
	}
	hooks := filepath.Join(shared, "hooks.json")
	owner := []byte("{\"owner\":true}\n")
	writeConfigFixture(t, hooks, owner, 0600)
	f.CheckTargets(t)
	if err := s.Install(t.Context(), plans[1]); err == nil || !strings.Contains(err.Error(), "changed after planning") {
		t.Fatalf("Codex install accepted an external change: %v", err)
	}
	if !bytes.Equal(configBytes(t, hooks), owner) {
		t.Fatal("the refused install wrote the owner's hooks file")
	}
}
