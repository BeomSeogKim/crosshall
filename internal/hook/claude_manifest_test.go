package hook

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
)

// The Claude Code manifest's install description is the data setup reads.
// Every installed event must have a runtime handler.
func TestClaudeManifestInstallDescription(t *testing.T) {
	raw, err := manifests.FS.ReadFile("claude.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Events  map[string]json.RawMessage `json:"events"`
		CapsMS  map[string]int64           `json:"caps_ms"`
		Install struct {
			HomeEnv  string            `json:"home_env"`
			File     string            `json:"file"`
			Encoding string            `json:"encoding"`
			Timeout  int               `json:"timeout_s"`
			Events   []string          `json:"events"`
			Matchers map[string]string `json:"matchers"`
		} `json:"install"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	in := m.Install
	if in.HomeEnv != "CLAUDE_CONFIG_DIR" || in.File != "settings.json" || in.Encoding != "argv" || in.Timeout != 1 {
		t.Fatalf("install target %+v", in)
	}
	want := []string{"SessionStart", "UserPromptSubmit", "Stop", "StopFailure", "Notification", "SessionEnd"}
	if !slices.Equal(in.Events, want) {
		t.Fatalf("installed events %v, want %v", in.Events, want)
	}
	for _, event := range in.Events {
		if _, ok := m.Events[event]; !ok {
			t.Errorf("installed event %s has no runtime handler", event)
		}
	}
	for event, matcher := range in.Matchers {
		if !slices.Contains(in.Events, event) || matcher == "" {
			t.Errorf("matcher %s=%q is not for an installed event", event, matcher)
		}
	}
	if in.Matchers["Notification"] != "permission_prompt" {
		t.Errorf("Notification matcher %q", in.Matchers["Notification"])
	}
	if len(m.CapsMS) != 3 {
		t.Errorf("freshness caps %v", m.CapsMS)
	}
}

// Claude Code: the running claude sets AI_AGENT and
// CLAUDE_CODE_ENTRYPOINT for its hooks; the profile carries what parses.
func TestClaudeLaunchVersionAndModeComeFromTheHookEnvironment(t *testing.T) {
	m, _ := loadManifest("claude")
	spec, _ := m.event("SessionStart")
	input := `{"hook_event_name":"SessionStart","session_id":"synthetic-session","source":"startup"}`
	for _, tc := range []struct {
		agent, entry, version, mode string
	}{
		{"claude-code_2-1-327_harness", "sdk-cli", "2.1.327", "print"},
		{"claude-code_2-1-329_agent", "cli", "2.1.329", "interactive"}, // observed in an interactive session
		{"claude-code_2-1-400_harness", "cli", "2.1.400", "interactive"},
		{"claude-code_2-1-329_agent", "sdk-cli", "2.1.329", "print"},
		{"claude-code_2-1-400", "cli", "unverified", "interactive"},      // an unobserved label form
		{"claude-code_2-1-329_agent", "vscode", "2.1.329", "unverified"}, // an unmapped entrypoint
		{"claude-code_latest", "sdk-cli", "unverified", "print"},
		{"claude-code_2-1-327-rc1_harness", "sdk-cli", "unverified", "print"}, // not the observed grammar
		{"claude-code_2-1-327_other", "sdk-cli", "unverified", "print"},
		{"", "", "unverified", "unverified"},
	} {
		lookup := func(name string) (string, bool) {
			switch name {
			case "CLAUDE_CODE_SESSION_ID":
				return "synthetic-session", true
			case "AI_AGENT":
				return tc.agent, tc.agent != ""
			case "CLAUDE_CODE_ENTRYPOINT":
				return tc.entry, tc.entry != ""
			}
			return "", false
		}
		e, err := parseEvent(strings.NewReader(input), m, spec, lookup)
		if err != nil {
			t.Fatal(err)
		}
		req := makeRequest(time.UnixMilli(1), m, spec, e, enrolment{}, true, 1)
		var args struct {
			Profile struct {
				Version string `json:"vendor_version"`
				Mode    string `json:"mode"`
			} `json:"profile"`
		}
		if err := json.Unmarshal(req.Args, &args); err != nil {
			t.Fatal(err)
		}
		if args.Profile.Version != tc.version || args.Profile.Mode != tc.mode {
			t.Errorf("AI_AGENT=%q entrypoint=%q: profile %+v", tc.agent, tc.entry, args.Profile)
		}
	}
}

// #163: two UserPromptSubmit callbacks of one turn id (a prompt typed while
// the turn runs) are two requests; a retry of one hook is the same request,
// and a reference too long for the dispatcher is replaced by its digest.
func TestPromptRequestsCarryTheHookEntryTime(t *testing.T) {
	m, _ := loadManifest("claude")
	spec, _ := m.event("UserPromptSubmit")
	request := func(turn string, at int64) (string, string) {
		t.Helper()
		input := `{"hook_event_name":"UserPromptSubmit","session_id":"s","prompt_id":"` + turn + `"}`
		e, err := parseEvent(strings.NewReader(input), m, spec, func(name string) (string, bool) {
			return "s", name == "CLAUDE_CODE_SESSION_ID"
		})
		if err != nil {
			t.Fatal(err)
		}
		req := makeRequest(time.UnixMilli(at), m, spec, e, enrolment{}, true, 1)
		var args struct {
			EventRef string `json:"event_ref"`
		}
		if err := json.Unmarshal(req.Args, &args); err != nil {
			t.Fatal(err)
		}
		return req.OpKey, args.EventRef
	}
	firstKey, firstRef := request("turn-a", 1000)
	retryKey, retryRef := request("turn-a", 1000)
	queuedKey, queuedRef := request("turn-a", 1700)
	if firstKey != retryKey || firstRef != retryRef {
		t.Fatalf("a retry changed the request: %q %q, %q %q", firstKey, firstRef, retryKey, retryRef)
	}
	if queuedKey == firstKey || queuedRef == firstRef {
		t.Fatalf("a second callback of the turn repeats the first: %q %q", queuedKey, queuedRef)
	}
	if _, ref := request(strings.Repeat("x", 111), 1000); len(ref) > 128 {
		t.Fatalf("event ref of %d bytes", len(ref))
	}
}

// Only a manifest that asks for it adds the entry time; Codex
// prompts keep their native reference, so a repeat stays a replay.
func TestOnlyClaudePromptsCarryTheEntryTime(t *testing.T) {
	m, _ := loadManifest("codex")
	spec, ok := m.event("UserPromptSubmit")
	if !ok {
		t.Skip("no codex prompt event")
	}
	input := `{"hook_event_name":"UserPromptSubmit","session_id":"s","turn_id":"turn-a"}`
	e, err := parseEvent(strings.NewReader(input), m, spec, func(name string) (string, bool) { return "s", name == "CODEX_THREAD_ID" })
	if err != nil {
		t.Fatal(err)
	}
	first := makeRequest(time.UnixMilli(1000), m, spec, e, enrolment{}, true, 1)
	second := makeRequest(time.UnixMilli(1700), m, spec, e, enrolment{}, true, 1)
	if first.OpKey != second.OpKey {
		t.Fatalf("codex prompt keys differ by entry time: %q %q", first.OpKey, second.OpKey)
	}
}
