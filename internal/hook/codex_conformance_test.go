package hook

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
)

func TestCodexInstallManifest(t *testing.T) {
	raw, err := manifests.FS.ReadFile("codex.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Install struct {
			HomeEnv  string            `json:"home_env"`
			File     string            `json:"file"`
			Encoding string            `json:"encoding"`
			Timeout  int               `json:"timeout_s"`
			Events   []string          `json:"events"`
			Matchers map[string]string `json:"matchers"`
		}
		CapsMS map[string]int64 `json:"caps_ms"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	i := m.Install
	if i.HomeEnv != "CODEX_HOME" || i.File != "hooks.json" || i.Encoding != "shell" || i.Timeout != 1 ||
		len(i.Matchers) != 0 || !slices.Equal(i.Events, []string{"SessionStart", "UserPromptSubmit", "Stop", "PermissionRequest", "SessionEnd"}) {
		t.Fatalf("unexpected Codex install description: %+v", i)
	}
	if !maps.Equal(m.CapsMS, map[string]int64{"prompt_submitted": 1_800_000, "turn_finishing": 7_200_000, "approval_requested": 1_800_000}) {
		t.Fatalf("unexpected fact freshness caps: %v", m.CapsMS)
	}
}

// Synthetic fields from the Codex exec callbacks. CI replays these without a vendor binary.
func TestCodexExecCallbacks(t *testing.T) {
	m, ok := loadManifest("codex")
	if !ok {
		t.Fatal("missing Codex manifest")
	}
	for _, tc := range []struct {
		name, fields, source, turn, fact, reason string
	}{
		{"SessionStart", `"cwd":"/fixture","source":"startup"`, "startup", "", "", ""},
		{"SessionStart", `"cwd":"/fixture","source":"resume"`, "resume", "", "", ""},
		{"UserPromptSubmit", `"turn_id":"turn-1","prompt":"synthetic text"`, "", "turn-1", "prompt_submitted", ""},
		{"Stop", `"turn_id":"turn-1","stop_hook_active":false`, "", "turn-1", "turn_finishing", ""},
		{"SessionEnd", `"reason":"other"`, "", "", "session_ended", "other"},
	} {
		t.Run(tc.name+"_"+tc.source, func(t *testing.T) {
			body := `{"session_id":"fixture-codex","hook_event_name":"` + tc.name + `",` + tc.fields + `}`
			spec, ok := m.event(tc.name)
			if !ok {
				t.Fatalf("missing event %s", tc.name)
			}
			got, err := parseEvent(strings.NewReader(body), m, spec, func(string) (string, bool) { return "", false })
			if err != nil || got.Identity.Vendor != "codex" || got.Identity.Key != "fixture-codex" ||
				got.Source != tc.source || got.TurnRef != tc.turn || got.Fact != tc.fact || got.Reason != tc.reason || got.LaunchRef != "" {
				t.Fatalf("unexpected callback mapping: %+v (%v)", got, err)
			}
		})
	}
}
