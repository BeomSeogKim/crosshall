package setup

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/setuptest"
)

// #385: Claude Code writes its settings after setup (a theme picked at first
// run), so uninstall cannot restore the original bytes. It must still remove
// the lists and objects setup created, and keep the vendor's edit.
func TestUninstallAfterAVendorEditRemovesWhatSetupCreated(t *testing.T) {
	for _, tc := range []struct {
		name, original, edit string
		want                 map[string]any
	}{
		{name: "empty hooks object", original: `{"hooks":{}}`, edit: `"theme": "auto", `,
			want: map[string]any{"hooks": map[string]any{}, "theme": "auto"}},
		{name: "no settings file", original: "", edit: `"theme": "auto", `,
			want: map[string]any{"theme": "auto"}},
		{name: "owner rule in the created list keeps the defaults", original: `{"hooks":{}}`,
			edit: `"autoMode": {"allow": ["Owner guidance"]}, `,
			want: map[string]any{"hooks": map[string]any{}, "autoMode": map[string]any{"allow": []any{"$defaults", "Owner guidance"}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setuptest.New(t, "claude")
			s := scopedService(f)
			path := filepath.Join(f.Home, ".claude", "settings.json")
			if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if tc.original != "" {
				writeConfigFixture(t, path, []byte(tc.original), 0600)
			}
			if out, err := realRun(t, s, f, Request{Terminal: true, Adapters: []string{"claude"}}, "y\n"); err != nil {
				t.Fatalf("setup failed: %v\n%s", err, out)
			}
			installed := configBytes(t, path)
			edited := vendorEdit(t, installed, tc.edit)
			writeConfigFixture(t, path, edited, 0600)

			preview, err := realRun(t, s, f, Request{Action: "uninstall", DryRun: true, Adapters: []string{"claude"}}, "")
			if err != nil || !strings.Contains(preview, "  remove hooks.Stop if empty\n") {
				t.Fatalf("uninstall preview does not name the emptied lists: %v\n%s", err, preview)
			}
			if out, err := realRun(t, s, f, Request{Action: "uninstall", Apply: true, Adapters: []string{"claude"}}, ""); err != nil {
				t.Fatalf("uninstall failed: %v\n%s", err, out)
			}
			var got map[string]any
			if err := json.Unmarshal(configBytes(t, path), &got); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("after uninstall = %v, want %v\ninstalled: %s", got, tc.want, installed)
			}
		})
	}
}

// vendorEdit inserts fields at the start of the settings object, or merges an
// autoMode edit into the list setup created, as Claude Code would rewrite it.
func vendorEdit(t *testing.T, installed []byte, fields string) []byte {
	t.Helper()
	if strings.HasPrefix(fields, `"autoMode"`) {
		edited := bytes.Replace(installed, []byte(`"allow":["$defaults",`), []byte(`"allow":["$defaults","Owner guidance",`), 1)
		if bytes.Equal(edited, installed) {
			t.Fatalf("fixture has no created autoMode list: %s", installed)
		}
		return edited
	}
	at := bytes.IndexByte(installed, '{')
	if at < 0 {
		t.Fatalf("settings are not an object: %s", installed)
	}
	return append(append(append([]byte{}, installed[:at+1]...), fields...), installed[at+1:]...)
}

// A partial uninstall keeps the changed entry's record but must not keep
// claiming the slots it pruned: a list the owner adds later stays.
func TestPartialUninstallReleasesThePrunedSlots(t *testing.T) {
	f := setuptest.New(t, "claude")
	s := scopedService(f)
	path := filepath.Join(f.Home, ".claude", "settings.json")
	if err := os.Mkdir(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	writeConfigFixture(t, path, []byte(`{"hooks":{}}`), 0600)
	if out, err := realRun(t, s, f, Request{Terminal: true, Adapters: []string{"claude"}}, "y\n"); err != nil {
		t.Fatalf("setup failed: %v\n%s", err, out)
	}
	installed := configBytes(t, path)
	start := bytes.Index(installed, []byte(`"SessionStart"`))
	timeout := regexp.MustCompile(`"timeout":\s*\d+`).FindIndex(installed[start:])
	if start < 0 || timeout == nil {
		t.Fatalf("no managed SessionStart timeout: %s", installed)
	}
	edited := slices.Concat(installed[:start+timeout[0]], []byte(`"timeout": 99`), installed[start+timeout[1]:])
	writeConfigFixture(t, path, edited, 0600)
	uninstall := func() map[string]any {
		t.Helper()
		if out, err := realRun(t, s, f, Request{Action: "uninstall", Apply: true, Adapters: []string{"claude"}}, ""); err != nil {
			t.Fatalf("uninstall failed: %v\n%s", err, out)
		}
		var got struct {
			Hooks map[string]any `json:"hooks"`
		}
		if err := json.Unmarshal(configBytes(t, path), &got); err != nil {
			t.Fatal(err)
		}
		return got.Hooks
	}
	if hooks := uninstall(); hooks["SessionStart"] == nil || hooks["Stop"] != nil {
		t.Fatalf("first uninstall hooks = %v, want only the changed SessionStart", hooks)
	}
	current := configBytes(t, path)
	owned := bytes.Replace(current, []byte(`"hooks":{`), []byte(`"hooks":{"Stop":[],`), 1)
	if bytes.Equal(owned, current) {
		t.Fatalf("fixture has no compact hooks object: %s", current)
	}
	writeConfigFixture(t, path, owned, 0600)
	if hooks := uninstall(); hooks["Stop"] == nil {
		t.Fatalf("retry removed the owner's empty Stop list: %v", hooks)
	}
}

func TestPruneRejectsAnEmptySlot(t *testing.T) {
	if _, err := pruneSlots([]byte(`{"hooks":{}}`), [][]string{{}}); err == nil {
		t.Fatal("an empty slot path from the journal was accepted")
	}
}
