package setup

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
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
