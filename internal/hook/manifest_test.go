package hook

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func parseFixture(adapter, name, body string, values map[string]string) (event, error) {
	m, exists := loadManifest(adapter)
	if !exists {
		return event{}, errHookInput
	}
	spec, exists := m.event(name)
	if !exists {
		return event{}, errHookInput
	}
	return parseEvent(strings.NewReader(body), m, spec, func(k string) (string, bool) {
		value, present := values[k]
		return value, present
	})
}

func TestHasPromptLineUsesTheInstalledManifest(t *testing.T) {
	for _, tc := range []struct {
		adapter, revision string
		want              bool
	}{
		{"claude", "claude-2", true},
		{"codex", "codex-1", true},
		{"agy", "agy-1", false},
		{"claude", "stale-revision", false},
		{"unknown", "unknown", false},
	} {
		if got := HasPromptLine(tc.adapter, tc.revision); got != tc.want {
			t.Errorf("%s %s: prompt line %t, want %t", tc.adapter, tc.revision, got, tc.want)
		}
	}
}

func TestManifestEvents(t *testing.T) {
	for _, adapter := range []string{"codex", "claude"} {
		m, ok := loadManifest(adapter)
		if !ok || m.Adapter != adapter || m.Revision == "" {
			t.Fatalf("missing manifest for %s", adapter)
		}
		start, _ := m.event("SessionStart")
		prompt, _ := m.event("UserPromptSubmit")
		stop, _ := m.event("Stop")
		end, _ := m.event("SessionEnd")
		if !start.Enroll || !prompt.Notice || !prompt.RecoverEnroll || !stop.Stop || stop.RecoverEnroll || end.Fact != "session_ended" {
			t.Errorf("incorrect effects for %s", adapter)
		}
		for _, unsupported := range []string{"PostToolUse", "SubagentStop", "owner"} {
			if _, ok := m.event(unsupported); ok {
				t.Errorf("unexpected %s event %s", adapter, unsupported)
			}
		}
	}
	if _, ok := loadManifest("../codex"); ok {
		t.Fatal("accepted an unregistered adapter")
	}
}

func TestHookIdentity(t *testing.T) {
	const body = `{"hook_event_name":"UserPromptSubmit","session_id":"fixture-session","turn_id":"fixture-turn"}`
	for _, tc := range []struct {
		name  string
		env   map[string]string
		valid bool
	}{
		{"payload only", nil, true},
		{"matching environment", map[string]string{"CODEX_THREAD_ID": "fixture-session"}, true},
		{"routing variable ignored", map[string]string{"CROSSHALL_ROUTE": "invalid"}, true},
		{"different session", map[string]string{"CODEX_THREAD_ID": "other"}, false},
		{"empty matching variable", map[string]string{"CODEX_THREAD_ID": ""}, false},
		{"mixed vendor", map[string]string{"CLAUDE_CODE_SESSION_ID": "parent"}, false},
		{"other recognized vendor even if empty", map[string]string{"ANTIGRAVITY_CONVERSATION_ID": ""}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseFixture("codex", "UserPromptSubmit", body, tc.env)
			if tc.valid {
				if err != nil || got.Identity.Vendor != "codex" || got.Identity.Key != "fixture-session" || got.TurnRef != "fixture-turn" || got.Fact != "prompt_submitted" {
					t.Fatalf("event=%+v err=%v", got, err)
				}
			} else if !errors.Is(err, errHookIdentity) {
				t.Fatalf("want identity failure, got %v", err)
			}
		})
	}
	for _, id := range []string{`null`, `""`, `"bad key"`, `"line\nnext"`, `123`} {
		_, err := parseFixture("codex", "UserPromptSubmit", strings.Replace(body, `"fixture-session"`, id, 1), nil)
		if !errors.Is(err, errHookIdentity) {
			t.Errorf("id=%s: %v", id, err)
		}
	}
}

func TestHookBoundedObject(t *testing.T) {
	const body = `{"hook_event_name":"SessionStart","session_id":"test-session","source":"startup","cwd":"/fixture"}`
	atLimit := body + strings.Repeat(" ", (64<<10)-len(body))
	got, err := parseFixture("claude", "SessionStart", atLimit, map[string]string{"CLAUDE_PID": "314"})
	if err != nil || got.Source != "startup" || got.CWD != "/fixture" || got.LaunchRef != "314" {
		t.Fatalf("at limit: %+v, %v", got, err)
	}
	for _, invalid := range []string{atLimit + " ", body + "{}", `[]`, `null`, strings.Replace(body, "SessionStart", "Stop", 1), strings.Replace(body, `"session_id"`, `"session_id":"first","session_id"`, 1)} {
		if _, err := parseFixture("claude", "SessionStart", invalid, nil); err == nil {
			t.Fatal("accepted malformed, ambiguous or oversized input")
		}
	}
}

func TestHookObservationFields(t *testing.T) {
	permission, err := parseFixture("codex", "PermissionRequest", `{"hook_event_name":"PermissionRequest","session_id":"fixture-session"}`, nil)
	if err != nil || permission.Fact != "approval_requested" || permission.TurnRef != "" {
		t.Fatalf("session-scoped approval: %+v, %v", permission, err)
	}
	const notification = `{"hook_event_name":"Notification","session_id":"room","notification_type":"permission_prompt","prompt_id":"turn1","message":"discard this"}`
	got, err := parseFixture("claude", "Notification", notification, nil)
	if err != nil || got.Fact != "approval_requested" || got.TurnRef != "turn1" {
		t.Fatalf("observation: %+v, %v", got, err)
	}
	if _, err := parseFixture("claude", "Notification", strings.Replace(notification, "permission_prompt", "idle_prompt", 1), nil); err == nil {
		t.Fatal("accepted unrelated notification")
	}
	ended, err := parseFixture("codex", "SessionEnd", `{"hook_event_name":"SessionEnd","session_id":"room","reason":"other"}`, nil)
	if err != nil || ended.Reason != "other" || ended.reference(time.UnixMilli(10)) == ended.reference(time.UnixMilli(11)) {
		t.Fatalf("end reference: %+v, %v", ended, err)
	}
	for _, test := range []struct{ vendor, event, key string }{{"claude", "Stop", "prompt_id"}, {"claude", "StopFailure", "prompt_id"}, {"codex", "Stop", "turn_id"}} {
		body := `{"hook_event_name":"` + test.event + `","session_id":"test","` + test.key + `":"turn1","stop_hook_active":true,"last_assistant_message":"discard this"}`
		e, err := parseFixture(test.vendor, test.event, body, nil)
		if err != nil || e.Fact != "turn_finishing" || e.TurnRef != "turn1" || e.reference(time.UnixMilli(1)) != e.reference(time.UnixMilli(2)) {
			t.Errorf("%+v: %+v, %v", test, e, err)
		}
	}
}

func TestHookDiscardsPrivateFields(t *testing.T) {
	const secret = "private-fixture-marker"
	const body = `{"hook_event_name":"UserPromptSubmit","session_id":"room","turn_id":"turn","prompt":"private-fixture-marker","cwd":"/private-fixture-marker","transcript_path":"/private-fixture-marker","tool_input":{"nested":["private-fixture-marker"]}}`
	got, err := parseFixture("codex", "UserPromptSubmit", body, nil)
	if got.CWD != "/"+secret {
		t.Fatal("prompt did not retain the local enrollment path")
	}
	m, _ := loadManifest("codex")
	spec, _ := m.event("UserPromptSubmit")
	req := makeRequest(time.Now(), m, spec, got, enrolment{}, true, 1)
	if strings.Contains(string(req.Args), secret) {
		t.Fatal("unmapped prompt data survived in the request")
	}
	got.CWD = ""
	if err != nil || strings.Contains(fmt.Sprintf("%+v", got), secret) {
		t.Fatal("private input changed the result or survived in normalized event")
	}
	for _, bad := range []string{body + secret, strings.Replace(body, `"room"`, `"private-fixture-marker invalid"`, 1)} {
		if _, err := parseFixture("codex", "UserPromptSubmit", bad, nil); err == nil || strings.Contains(err.Error(), secret) {
			t.Fatal("diagnostic retained private input or invalid input passed")
		}
	}
}

func TestInvalidPromptCWDLeavesObservationIntact(t *testing.T) {
	for _, fields := range []string{`"cwd":[]`, `"cwd":"relative"`, `"cwd":"/first","cwd":"/second"`} {
		body := `{"hook_event_name":"UserPromptSubmit","session_id":"thread","turn_id":"turn",` + fields + `}`
		event, err := parseFixture("codex", "UserPromptSubmit", body, nil)
		if err != nil || event.CWD != "" || event.EventRef != "turn:UserPromptSubmit" {
			t.Fatalf("invalid optional cwd altered the prompt observation: %+v (%v)", event, err)
		}
	}
}

func TestInvalidLaunchVariableFallsBackToTheSession(t *testing.T) {
	m, ok := loadManifest("claude")
	spec, known := m.event("SessionStart")
	if !ok || !known {
		t.Fatal("claude SessionStart manifest")
	}
	lookup := func(name string) (string, bool) {
		switch name {
		case "CLAUDE_CODE_SESSION_ID":
			return "synthetic-session", true
		case "CLAUDE_PID":
			return "abc", true
		}
		return "", false
	}
	e, err := parseEvent(strings.NewReader(`{"hook_event_name":"SessionStart","session_id":"synthetic-session","source":"startup"}`), m, spec, lookup)
	if err != nil || e.LaunchRef != "" {
		t.Fatalf("invalid CLAUDE_PID: launch %q, error %v", e.LaunchRef, err)
	}
}
