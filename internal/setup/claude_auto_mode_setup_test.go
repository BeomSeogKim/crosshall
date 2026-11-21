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
		{name: "edited settings retain defaults", original: "{\n \"owner\": 1\n}\n", before: []string{"$defaults"}, edit: true},
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
			if err != nil || j.Version != 3 || !slices.ContainsFunc(j.Files["claude"].Entries, func(e Entry) bool { return e.AutoMode }) {
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
				if got := autoModeAllowed(t, restored); !slices.Equal(got, tc.before) || !bytes.Contains(restored, []byte(`"owner": 2`)) {
					t.Fatalf("semantic uninstall changed defaults or owner edits: %s", restored)
				}
			} else if string(restored) != tc.original {
				t.Fatalf("uninstall did not restore original bytes: %q", restored)
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
