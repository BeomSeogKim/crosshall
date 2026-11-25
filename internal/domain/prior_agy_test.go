package domain

import (
	"encoding/json"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
)

// withSessionStartOnlyAgy serves the Antigravity manifest as it was at
// agy-1, SessionStart only, for checks of an adapter without a turn end.
func withSessionStartOnlyAgy(t *testing.T) {
	t.Helper()
	raw, err := manifests.FS.ReadFile("agy.json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["revision"] = "agy-1"
	delete(m, "prior_revisions")
	events := m["events"].(map[string]any)
	m["events"] = map[string]any{"SessionStart": events["SessionStart"]}
	m["install"].(map[string]any)["events"] = []string{"SessionStart"}
	old, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	restore := manifests.SwapReader(func(adapter string) ([]byte, error) {
		if adapter == "agy" {
			return old, nil
		}
		return manifests.FS.ReadFile(adapter + ".json")
	})
	t.Cleanup(restore)
}
