package setup

import (
	"bytes"
	"encoding/json"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/setuptest"
)

func TestSkillFrontmatterNamesCrosshallWithAShortDescription(t *testing.T) {
	text, ok := strings.CutPrefix(string(skillContent), "---\n")
	front, body, closed := strings.Cut(text, "\n---\n")
	if !ok || !closed || strings.TrimSpace(body) == "" {
		t.Fatalf("skill has no frontmatter block and body:\n%s", skillContent)
	}
	keys := map[string]string{}
	for line := range strings.SplitSeq(front, "\n") {
		key, value, found := strings.Cut(line, ": ")
		if !found || key == "" || strings.HasPrefix(key, " ") {
			t.Fatalf("frontmatter line %q is not a key: value pair", line)
		}
		keys[key] = value
	}
	if len(keys) != 2 || keys["name"] != "crosshall" {
		t.Fatalf("frontmatter keys = %v, want only name: crosshall and description", keys)
	}
	if description := keys["description"]; description == "" || len(description) >= 1024 {
		t.Fatalf("description has %d characters, want 1 to 1023", len(description))
	}
}

// homeTree records every directory and file under root, with file bytes
// and modes, so an uninstall can be compared byte for byte.
func homeTree(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		info, err := d.Info()
		if err != nil {
			return err
		}
		if d.IsDir() {
			tree[rel] = "dir " + info.Mode().String()
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		tree[rel] = info.Mode().String() + " " + string(raw)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

func vendorSkill(t *testing.T, plan VendorPlan) PlanFile {
	t.Helper()
	for _, f := range plan.Files {
		if f.Skill != "" {
			return f
		}
	}
	t.Fatalf("plan has no skill file: %+v", plan.Files)
	return PlanFile{}
}

func installVendor(t *testing.T, f setuptest.Fixture, s Service, adapter string) VendorPlan {
	t.Helper()
	plans, err := s.PlanInstall(t.Context(), []string{adapter})
	if err != nil || len(plans) != 1 || plans[0].Err != nil {
		t.Fatalf("%s setup plan: %+v (%v)", adapter, plans, err)
	}
	f.CheckTargets(t)
	if err := s.Install(t.Context(), plans[0]); err != nil {
		t.Fatal(err)
	}
	return plans[0]
}

func uninstallVendor(t *testing.T, f setuptest.Fixture, s Service, adapter string) VendorPlan {
	t.Helper()
	plans, err := s.PlanUninstall(t.Context(), []string{adapter})
	if err != nil || len(plans) != 1 || plans[0].Err != nil {
		t.Fatalf("%s uninstall plan: %+v (%v)", adapter, plans, err)
	}
	f.CheckTargets(t)
	if err := s.Uninstall(t.Context(), plans[0]); err != nil {
		t.Fatal(err)
	}
	return plans[0]
}

func TestSkillInstallAndUninstallAreByteExact(t *testing.T) {
	for _, tc := range []struct {
		name, adapter, home string
		vendorHome, skills  bool
	}{
		{name: "Claude Code", adapter: "claude", home: ".claude", vendorHome: true},
		{name: "Claude Code with other skills", adapter: "claude", home: ".claude", vendorHome: true, skills: true},
		{name: "Codex", adapter: "codex", home: ".codex", vendorHome: true},
		{name: "Codex with other skills", adapter: "codex", home: ".codex", vendorHome: true, skills: true},
		{name: "Antigravity fresh home", adapter: "agy", home: ".gemini/config"},
		{name: "Antigravity with other skills", adapter: "agy", home: ".gemini/config", vendorHome: true, skills: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setuptest.New(t, tc.adapter)
			s := scopedService(f)
			home := filepath.Join(f.Home, tc.home)
			path := filepath.Join(home, "skills", "crosshall", "SKILL.md")
			if tc.vendorHome {
				if err := os.MkdirAll(home, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if tc.skills {
				other := filepath.Join(home, "skills", "other")
				if err := os.MkdirAll(other, 0755); err != nil {
					t.Fatal(err)
				}
				writeConfigFixture(t, filepath.Join(other, "SKILL.md"), []byte("---\nname: other\n---\n"), 0644)
			}
			before := homeTree(t, f.Home)

			plan := installVendor(t, f, s, tc.adapter)
			if skill := vendorSkill(t, plan); skill.Skill != "install" || skill.Path != path {
				t.Fatalf("skill plan = %+v, want install of %s", skill, path)
			}
			var preview bytes.Buffer
			if err := RenderVendorPlan(&preview, plan); err != nil || !strings.Contains(preview.String(), "  install skill "+path+"\n") {
				t.Fatalf("plan does not show the skill: %v\n%s", err, preview.String())
			}
			installed, err := readConfig(path)
			if err != nil || !bytes.Equal(installed.Bytes, skillContent) || installed.Mode != skillMode {
				t.Fatalf("installed skill = %+v (%v)", installed, err)
			}
			j, _, err := readJournal(f.State)
			if err != nil || j.Version != skillJournalVersion || j.SkillFiles[tc.adapter].Path != path {
				t.Fatalf("journal skill record: %+v (%v)", j.SkillFiles, err)
			}

			removal := uninstallVendor(t, f, s, tc.adapter)
			if skill := vendorSkill(t, removal); skill.Skill != "remove" || skill.Path != path {
				t.Fatalf("skill removal plan = %+v", skill)
			}
			if after := homeTree(t, f.Home); !maps.Equal(before, after) {
				t.Fatalf("uninstall left the home changed:\nbefore %v\nafter  %v", before, after)
			}
			if j, _, err := readJournal(f.State); err != nil || len(j.SkillFiles) != 0 || j.Pending != nil {
				t.Fatalf("journal after uninstall: %+v (%v)", j, err)
			}
		})
	}
}

func TestSkillEditedByTheOwnerIsKeptOnReinstallAndUninstall(t *testing.T) {
	f := setuptest.New(t, "codex")
	s := scopedService(f)
	path := filepath.Join(f.Home, ".codex", "skills", "crosshall", "SKILL.md")
	var out bytes.Buffer
	setup := func() error {
		out.Reset()
		f.CheckTargets(t)
		return s.Run(t.Context(), Request{Action: "setup", Adapters: []string{"codex"}, Apply: true,
			Input: strings.NewReader(""), Output: &out})
	}
	if err := setup(); err != nil || !strings.Contains(out.String(), "installed the crosshall skill for Codex ("+path+")\n") {
		t.Fatalf("setup did not report the installed skill: %v\n%s", err, out.String())
	}
	edited := append(bytes.Clone(skillContent), "\nOwner note.\n"...)
	writeConfigFixture(t, path, edited, 0644)

	err := setup()
	if err != nil || !strings.Contains(out.String(), skillEditedReason+" ("+path+")") {
		t.Fatalf("reinstall did not report the kept skill: %v\n%s", err, out.String())
	}
	if got := configBytes(t, path); !bytes.Equal(got, edited) {
		t.Fatalf("reinstall replaced the edited skill: %q", got)
	}
	if !bytes.Contains(configBytes(t, filepath.Join(f.Home, ".codex", "hooks.json")), []byte("SessionStart")) {
		t.Fatal("the edited skill blocked the hooks")
	}

	plans, err := s.PlanUninstall(t.Context(), []string{"codex"})
	if err != nil || len(plans) != 1 || len(plans[0].Retained) != 1 || plans[0].Retained[0].Reason != skillEditedReason {
		t.Fatalf("uninstall plan does not keep the edited skill: %+v (%v)", plans, err)
	}
	f.CheckTargets(t)
	if err := s.Uninstall(t.Context(), plans[0]); err != nil {
		t.Fatal(err)
	}
	if got := configBytes(t, path); !bytes.Equal(got, edited) {
		t.Fatalf("uninstall removed or changed the edited skill: %q", got)
	}
	if j, _, err := readJournal(f.State); err != nil || len(j.SkillFiles) != 0 || len(j.Files) != 0 {
		t.Fatalf("uninstall kept ownership: %+v (%v)", j, err)
	}
}

func TestSkillLeavesAFileSetupDidNotWrite(t *testing.T) {
	for _, tc := range []struct {
		name     string
		existing []byte
		retained bool
	}{
		{name: "different file", existing: []byte("---\nname: crosshall\ndescription: mine\n---\n"), retained: true},
		{name: "identical file", existing: skillContent},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setuptest.New(t, "claude")
			s := scopedService(f)
			path := filepath.Join(f.Home, ".claude", "skills", "crosshall", "SKILL.md")
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			writeConfigFixture(t, path, tc.existing, 0600)
			plan := installVendor(t, f, s, "claude")
			for _, file := range plan.Files {
				if file.Skill != "" {
					t.Fatalf("plan writes an existing skill: %+v", file)
				}
			}
			if retained := len(plan.Retained) == 1 && plan.Retained[0].Path == path; retained != tc.retained {
				t.Fatalf("retained = %+v, want retained %t", plan.Retained, tc.retained)
			}
			if j, _, err := readJournal(f.State); err != nil || len(j.SkillFiles) != 0 {
				t.Fatalf("setup recorded a skill it did not write: %+v (%v)", j.SkillFiles, err)
			}
			uninstallVendor(t, f, s, "claude")
			if got, err := readConfig(path); err != nil || !bytes.Equal(got.Bytes, tc.existing) || got.Mode != 0600 {
				t.Fatalf("uninstall touched the owner's skill: %+v (%v)", got, err)
			}
		})
	}
}

func TestSkillUpgradeReplacesAnUneditedFile(t *testing.T) {
	f := setuptest.New(t, "agy")
	s := scopedService(f)
	path := filepath.Join(f.Home, ".gemini", "config", "skills", "crosshall", "SKILL.md")
	installVendor(t, f, s, "agy")

	// An earlier release wrote, and recorded, other bytes.
	older := []byte("---\nname: crosshall\ndescription: an earlier release\n---\n")
	writeConfigFixture(t, path, older, 0644)
	snapshot, err := readConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	j, _, err := readJournal(f.State)
	if err != nil {
		t.Fatal(err)
	}
	record := j.SkillFiles["agy"]
	record.Latest = stamp(snapshot)
	j.SkillFiles["agy"] = record
	writeStatusJournal(t, f, j)

	plans, err := s.PlanInstall(t.Context(), []string{"agy"})
	plan := requireAgyPlan(t, plans, err)
	if skill := vendorSkill(t, plan); skill.Skill != "update" || len(plan.Retained) != 0 {
		t.Fatalf("upgrade plan = %+v, retained %+v", skill, plan.Retained)
	}
	f.CheckTargets(t)
	if err := s.Install(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
	if got := configBytes(t, path); !bytes.Equal(got, skillContent) {
		t.Fatalf("upgrade left %q", got)
	}
	if j, _, err := readJournal(f.State); err != nil || j.SkillFiles["agy"].Latest.Hash != sum(skillContent) {
		t.Fatalf("upgrade did not record the new bytes: %+v (%v)", j.SkillFiles, err)
	}
	uninstallVendor(t, f, s, "agy")
	if _, err := os.Lstat(filepath.Join(f.Home, ".gemini")); !os.IsNotExist(err) {
		t.Fatalf("uninstall after an upgrade left the created home: %v", err)
	}
}

func TestSkillFollowsTheVendorHomeOverrides(t *testing.T) {
	f := setuptest.New(t, "claude", "codex")
	s := scopedService(f)
	claudeHome := filepath.Join(f.Root, "claude config")
	codexHome := filepath.Join(f.Root, "codex config")
	t.Setenv("CLAUDE_CONFIG_DIR", claudeHome)
	t.Setenv("CODEX_HOME", codexHome)
	for adapter, home := range map[string]string{"claude": claudeHome, "codex": codexHome} {
		plan := installVendor(t, f, s, adapter)
		path := filepath.Join(home, "skills", "crosshall", "SKILL.md")
		if skill := vendorSkill(t, plan); skill.Path != path || !bytes.Equal(configBytes(t, path), skillContent) {
			t.Fatalf("%s skill = %+v", adapter, skill)
		}
	}
	for _, dir := range []string{".claude", ".codex"} {
		if _, err := os.Lstat(filepath.Join(f.Home, dir)); !os.IsNotExist(err) {
			t.Fatalf("setup wrote under the default home %s: %v", dir, err)
		}
	}
}

func TestSkillInstallsOverAJournalOfThePreviousVersion(t *testing.T) {
	f := setuptest.New(t, "claude")
	s := scopedService(f)
	path := filepath.Join(f.Home, ".claude", "skills", "crosshall", "SKILL.md")
	installVendor(t, f, s, "claude")

	// Rewrite the state as the previous release left it: hooks, no skill.
	j, _, err := readJournal(f.State)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{path, filepath.Dir(path), filepath.Dir(filepath.Dir(path))} {
		if err := os.Remove(name); err != nil {
			t.Fatal(err)
		}
	}
	j.SkillFiles = map[string]ownedFile{}
	j.Version = classifierJournalVersion
	raw, err := json.Marshal(j)
	if err != nil || bytes.Contains(raw, []byte("skill")) {
		t.Fatalf("previous journal = %s (%v)", raw, err)
	}
	f.CheckTargets(t)
	if err := os.WriteFile(filepath.Join(f.State, journalName), raw, 0600); err != nil {
		t.Fatal(err)
	}
	previous, _, err := readJournal(f.State)
	if err != nil || previous.Version != classifierJournalVersion || previous.SkillFiles == nil || len(previous.Files) != 1 {
		t.Fatalf("read the previous journal: %+v (%v)", previous, err)
	}

	plan := installVendor(t, f, s, "claude")
	if skill := vendorSkill(t, plan); skill.Skill != "install" {
		t.Fatalf("skill plan over the previous journal = %+v", skill)
	}
	if got, _, err := readJournal(f.State); err != nil || got.Version != skillJournalVersion || got.SkillFiles["claude"].Path != path {
		t.Fatalf("journal after the skill install: %+v (%v)", got, err)
	}
	if !bytes.Equal(configBytes(t, path), skillContent) {
		t.Fatal("skill not installed")
	}
}
