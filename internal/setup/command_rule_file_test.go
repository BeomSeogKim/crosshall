package setup

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/setuptest"
)

func TestUninstallCodexCommandRulesPreservesOwnerEdits(t *testing.T) {
	for _, tc := range []struct {
		name      string
		replace   bool
		preexists bool
	}{
		{name: "appended rule"},
		{name: "replaced file", replace: true},
		{name: "identical preexisting rules", preexists: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setuptest.New(t, "codex")
			s := scopedService(f)
			path := filepath.Join(f.Home, ".codex", "rules", "crosshall.rules")
			_, rules, err := memberCommandRules("codex", s.Executable)
			if err != nil {
				t.Fatal(err)
			}
			if tc.preexists {
				if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
					t.Fatal(err)
				}
				writeConfigFixture(t, path, rules, 0600)
			}
			f.CheckTargets(t)
			install := codexInstallPlan(t, s)
			if err := s.Install(t.Context(), install); err != nil {
				t.Fatal(err)
			}
			if got := configBytes(t, path); !bytes.Equal(got, rules) {
				t.Fatalf("installed rules = %q, want %q", got, rules)
			}

			owner := []byte("prefix_rule(pattern=[\"/owner/bin/check\"], decision=\"prompt\")\n")
			want := owner
			switch {
			case tc.preexists:
				want = rules
			case tc.replace:
				replacement := path + ".editor"
				want = slices.Concat(rules, owner)
				writeConfigFixture(t, replacement, want, 0600)
				if err := os.Rename(replacement, path); err != nil {
					t.Fatal(err)
				}
			default:
				writeConfigFixture(t, path, slices.Concat(rules, owner), 0600)
			}
			plans, err := s.PlanUninstall(t.Context(), []string{"codex"})
			if err != nil || len(plans) != 1 || plans[0].Err != nil || (len(plans[0].Retained) != 0) != tc.replace {
				t.Fatalf("retained rules in uninstall plan: %+v (%v)", plans, err)
			}
			if err := s.Uninstall(t.Context(), plans[0]); err != nil {
				t.Fatal(err)
			}
			if got := configBytes(t, path); !bytes.Equal(got, want) {
				t.Fatalf("uninstall left %q, want %q", got, want)
			}
		})
	}
}

func TestCommandRuleFileRecordsAndCleansOnlyCreatedDirectories(t *testing.T) {
	for _, tc := range []struct {
		name, vendorHome, file string
		preexisting            bool
	}{
		{name: "Codex", vendorHome: ".codex", file: "rules/crosshall.rules"},
		{name: "Codex repeated directory name", vendorHome: "rules", file: "rules/crosshall.rules"},
		{name: "Codex preexisting home", vendorHome: ".codex", file: "rules/crosshall.rules", preexisting: true},
		{name: "Antigravity", vendorHome: ".gemini", file: "antigravity-cli/settings.json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), tc.vendorHome)
			path := filepath.Join(home, tc.file)
			ownerFile := filepath.Join(home, "owner-file")
			if tc.preexisting {
				if err := os.Mkdir(home, 0700); err != nil {
					t.Fatal(err)
				}
				writeConfigFixture(t, ownerFile, []byte("owner"), 0600)
			}
			before, err := readConfig(path)
			if err != nil || before.Exists {
				t.Fatalf("missing file snapshot = %+v, %v", before, err)
			}
			var created []createdConfigDir
			if err := replaceConfigJournaled(path, before, []byte("{}\n"), 0600, false, nil, func(dir createdConfigDir) error {
				created = append(created, dir)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			want := []string{tc.vendorHome, filepath.Base(filepath.Dir(path))}
			if tc.preexisting {
				want = want[1:]
			}
			var names []string
			for _, dir := range created {
				names = append(names, dir.Name)
			}
			if !slices.Equal(names, want) {
				t.Fatalf("created %v, want %v", names, want)
			}
			after, err := readConfig(path)
			if err != nil || !after.Exists || string(after.Bytes) != "{}\n" || after.Mode != 0600 {
				t.Fatalf("installed file snapshot = %+v, %v", after, err)
			}
			if err := replaceConfig(path, after, nil, after.Mode, true); err != nil {
				t.Fatal(err)
			}
			for _, dir := range slices.Backward(created) {
				if err := removeCreatedConfigDir(path, dir); err != nil {
					t.Fatal(err)
				}
				absent, err := readConfig(path)
				if err != nil || absent.Exists || !configCleanupMatchesAbsence(path, absent.DirIdentity, dir) {
					t.Fatalf("missing file after cleaning %+v: %+v (%v)", dir, absent, err)
				}
			}
			if _, err := os.Lstat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("created rule directory remained: %v", err)
			}
			if tc.preexisting {
				if raw, err := os.ReadFile(ownerFile); err != nil || string(raw) != "owner" {
					t.Fatalf("owner file = %q (%v)", raw, err)
				}
			} else if _, err := os.Lstat(home); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("created vendor home remained: %v", err)
			}
		})
	}
}

func TestCommandRuleFileRefusesUnmanagedMissingOrLinkedAncestors(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "unmanaged", "codex-home", "rules", "crosshall.rules")
	if _, err := readConfig(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unmanaged missing ancestor error = %v", err)
	}
	if err := replaceConfig(path, fileSnapshot{}, []byte("test"), 0600, false); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unmanaged missing ancestor replacement error = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "unmanaged")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unmanaged ancestor was created: %v", err)
	}
	outside := t.TempDir()
	outsideFile := filepath.Join(outside, "settings.json")
	writeConfigFixture(t, outsideFile, []byte("owner"), 0600)
	gemini := filepath.Join(root, ".gemini")
	if err := os.Mkdir(gemini, 0700); err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(gemini, "antigravity-cli")
	if err := os.Symlink(outside, linked); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(linked, "settings.json")
	if _, err := readConfig(path); err == nil {
		t.Fatal("rule read followed a symlink")
	}
	if err := replaceConfig(path, fileSnapshot{}, []byte("test"), 0600, false); err == nil {
		t.Fatal("rule replacement followed a symlink")
	}
	if raw, err := os.ReadFile(outsideFile); err != nil || string(raw) != "owner" {
		t.Fatalf("owner file = %q (%v)", raw, err)
	}
}
