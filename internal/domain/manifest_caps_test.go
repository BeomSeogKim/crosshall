package domain

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// The manifests ship the freshness caps as data; the domain applies the same
// values. A change to one without the other fails here.
func TestManifestCapsMatchTheDomain(t *testing.T) {
	for _, adapter := range []string{"claude", "codex"} {
		raw, err := os.ReadFile("../hook/manifests/" + adapter + ".json")
		if err != nil {
			t.Fatal(err)
		}
		var m struct {
			CapsMS map[string]int64 `json:"caps_ms"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(m.CapsMS, factCapMS) {
			t.Errorf("%s manifest caps %v, domain caps %v", adapter, m.CapsMS, factCapMS)
		}
	}
}
