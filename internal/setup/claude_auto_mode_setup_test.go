package setup

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/setuptest"
)

func TestClaudeAutoModeRulesShareSetupConsentAndRestoreSettings(t *testing.T) {
	for _, tc := range []struct {
		name, original string
		before         []string
		edit           bool
	}{
		{name: "missing list", original: "{\n \"owner\": 1, \"autoMode\": {\"hard_deny\": [\"Keep protected data local\"]}\n}\n", before: []string{"$defaults"}},
		{name: "explicit defaults", original: "{\n \"autoMode\": {\"allow\": [\"Owner guidance\", \"$defaults\"]}\n}\n",
			before: []string{"Owner guidance", "$defaults"}},
		{name: "owner replaces defaults", original: "{\n \"autoMode\": {\"allow\": [\"Owner guidance\"]}\n}\n",
			before: []string{"Owner guidance"}},
		// Setup created the list, so an uninstall after an owner edit removes
		// it with $defaults, as restoring the original bytes would (#385).
		{name: "edited settings drop the list setup created", original: "{\n \"owner\": 1\n}\n", before: []string{"$defaults"}, edit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setuptest.New(t, "claude")
			s := scopedService(f)
			path := filepath.Join(f.Home, ".claude", "settings.json")
			if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			writeConfigFixture(t, path, []byte(tc.original), 0640)
			req := Request{Terminal: true, Adapters: []string{"claude"}}
			guidance := claudeAutoModeEntries(s.Executable, false)[0]
			preview := func(answer string) {
				t.Helper()
				out, err := realRun(t, s, f, req, answer)
				shown, _, question := strings.Cut(out, "Install the listed hooks and command rules? [y/N]")
				if err != nil || !question || strings.Count(out, "[y/N]") != 1 ||
					!strings.Contains(shown, "add autoMode.allow "+string(guidance.Value)) {
					t.Fatalf("classifier rule consent: %v\n%s", err, out)
				}
			}
			preview("n\n")
			if got := configBytes(t, path); string(got) != tc.original {
				t.Fatalf("decline changed settings: %q", got)
			}
			preview("y\n")
			installed := configBytes(t, path)
			var rule string
			if err := json.Unmarshal(guidance.Value, &rule); err != nil {
				t.Fatal(err)
			}
			want := append(slices.Clone(tc.before), rule)
			if got := autoModeAllowed(t, installed); !slices.Equal(got, want) {
				t.Fatalf("installed classifier allow list = %q, want %q", got, want)
			}
			j, _, err := readJournal(f.State)
			if err != nil || j.Version != skillJournalVersion || !slices.ContainsFunc(j.Files["claude"].Entries, func(e Entry) bool { return e.AutoMode }) {
				t.Fatalf("classifier journal = %+v (%v)", j, err)
			}
			if out, err := realRun(t, s, f, Request{Apply: true, Adapters: req.Adapters}, ""); err != nil {
				t.Fatalf("reinstall failed: %v\n%s", err, out)
			}
			if data := configBytes(t, path); !bytes.Equal(data, installed) {
				t.Fatalf("reinstall changed classifier settings: %s", data)
			}
			if tc.edit {
				edited := bytes.Replace(installed, []byte(`"owner": 1`), []byte(`"owner": 2`), 1)
				writeConfigFixture(t, path, edited, 0640)
			}
			if out, err := realRun(t, s, f, Request{Action: "uninstall", Apply: true, Adapters: req.Adapters}, ""); err != nil {
				t.Fatalf("uninstall failed: %v\n%s", err, out)
			}
			restored := configBytes(t, path)
			if tc.edit {
				if bytes.Contains(restored, []byte(`"autoMode"`)) || !bytes.Contains(restored, []byte(`"owner": 2`)) {
					t.Fatalf("semantic uninstall changed defaults or owner edits: %s", restored)
				}
			} else if string(restored) != tc.original {
				t.Fatalf("uninstall did not restore original bytes: %q", restored)
			}
		})
	}
}

func TestClaudeAutoModeUninstallReleasesAbsentClaims(t *testing.T) {
	for _, tc := range []struct {
		name          string
		duplicate     bool
		keepCodexRule bool
	}{
		{name: "owner changes the classifier rule"},
		{name: "owner changes the classifier rule while Codex stays installed", keepCodexRule: true},
		{name: "owner duplicates the exact classifier rule", duplicate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapters := []string{"claude"}
			cleanVersion := 1
			if tc.keepCodexRule {
				adapters = append(adapters, "codex")
				cleanVersion = skillJournalVersion // Codex keeps its skill
			}
			f := setuptest.New(t, adapters...)
			s := scopedService(f)
			path := filepath.Join(f.Home, ".claude", "settings.json")
			if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			const original = "{\n \"owner\": 1, \"autoMode\": {\"allow\": [\"$defaults\", \"Owner guidance\"], \"hard_deny\": [\"Keep protected data local\"]}\n}\n"
			writeConfigFixture(t, path, []byte(original), 0640)
			if out, err := realRun(t, s, f, Request{Terminal: true, Adapters: adapters}, "y\n"); err != nil {
				t.Fatalf("setup failed: %v\n%s", err, out)
			}
			guidance := claudeAutoModeEntries(s.Executable, false)[0]
			var rule string
			if err := json.Unmarshal(guidance.Value, &rule); err != nil {
				t.Fatal(err)
			}
			checkJournal := func(version int, claudeOwned bool) ownedFile {
				t.Helper()
				j, _, err := readJournal(f.State)
				record, hasClaude := j.Files["claude"]
				_, hasCodexRule := j.RuleFiles["codex"]
				if err != nil || j.Version != version || j.Pending != nil || hasClaude != claudeOwned || hasCodexRule != tc.keepCodexRule {
					t.Fatalf("want journal v%d with Claude ownership %t and Codex rule ownership %t, got %+v (%v)",
						version, claudeOwned, tc.keepCodexRule, j, err)
				}
				return record
			}
			checkJournal(skillJournalVersion, true)

			installed := configBytes(t, path)
			if bytes.Count(installed, guidance.Value) != 1 {
				t.Fatalf("fixture must own exactly one original classifier rule: %s", installed)
			}
			const ownerRule = "Owner replacement classifier"
			ownerValue, err := json.Marshal(ownerRule)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"$defaults", "Owner guidance", ownerRule}
			if tc.duplicate {
				ownerValue = bytes.Join([][]byte{guidance.Value, guidance.Value}, []byte(", "))
				want = []string{"$defaults", "Owner guidance", rule, rule}
			}
			edited := bytes.Replace(installed, guidance.Value, ownerValue, 1)
			writeConfigFixture(t, path, edited, 0640)
			uninstall := func() {
				t.Helper()
				if out, err := realRun(t, s, f, Request{Action: "uninstall", Apply: true, Adapters: []string{"claude"}}, ""); err != nil {
					t.Fatalf("uninstall failed: %v\n%s", err, out)
				}
			}
			uninstall()
			after := configBytes(t, path)
			if got := autoModeAllowed(t, after); !slices.Equal(got, want) || !bytes.Contains(after, []byte(`"hard_deny": ["Keep protected data local"]`)) {
				t.Fatalf("uninstall did not preserve the owner settings and rules %q: %s", want, after)
			}
			if tc.duplicate {
				record := checkJournal(classifierJournalVersion, true)
				if len(record.Entries) != 1 || !record.Entries[0].AutoMode || !bytes.Equal(record.Entries[0].Value, guidance.Value) {
					t.Fatalf("uninstall did not keep just the ambiguous original claim: %+v", record.Entries)
				}
			} else {
				checkJournal(cleanVersion, false)
			}

			ownerCleaned := bytes.Replace([]byte(original), []byte(`"owner": 1`), []byte(`"owner": 2`), 1)
			writeConfigFixture(t, path, ownerCleaned, 0640)
			before, err := os.Lstat(path)
			if err != nil {
				t.Fatal(err)
			}
			for attempt := 1; attempt <= 2; attempt++ {
				uninstall()
				checkJournal(cleanVersion, false)
				after, err := os.Lstat(path)
				if err != nil || !os.SameFile(before, after) || !bytes.Equal(configBytes(t, path), ownerCleaned) {
					t.Fatalf("uninstall %d replaced or changed settings after owner cleanup: %v", attempt, err)
				}
			}
		})
	}
}

func autoModeAllowed(t *testing.T, raw []byte) []string {
	t.Helper()
	var settings struct {
		AutoMode struct {
			Allow []string
		} `json:"autoMode"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		t.Fatal(err)
	}
	return settings.AutoMode.Allow
}
