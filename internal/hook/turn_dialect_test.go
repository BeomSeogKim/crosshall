package hook

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// A vendor whose prompt callback fires once per model call, names its turn
// only by a step count, and whose turn end names no turn (#328).
const perCallDialect = `{
  "schema": 1, "adapter": "agy", "revision": "dialect-test",
  "event_field": "hookEventName", "session_field": "conversationId", "session_env": "ANTIGRAVITY_CONVERSATION_ID",
  "literal_fields": ["invocationNum", "initialNumSteps", "executionNum", "fullyIdle"],
  "stop_active": {"field": "executionNum", "inactive": "0"},
  "events": {
    "PreInvocation": {"fact": "prompt_submitted", "notice": true, "output": "inject_steps", "turn_required": true,
      "when": {"invocationNum": "0"}, "turn_field": "initialNumSteps", "open_turn": true},
    "Stop": {"fact": "turn_finishing", "stop": true, "output": "continue", "turn_from_open": true,
      "ref_fields": ["executionNum"], "stop_require": {"terminationReason": "NO_TOOL_CALL", "fullyIdle": "true", "error": ""}}
  }
}`

func perCallManifest(t *testing.T) manifest {
	t.Helper()
	var m manifest
	if err := json.Unmarshal([]byte(perCallDialect), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func perCallEvent(t *testing.T, m manifest, name string, fields map[string]any) (event, error) {
	t.Helper()
	spec, ok := m.event(name)
	if !ok {
		t.Fatalf("no %s event", name)
	}
	payload := map[string]any{"hookEventName": name, "conversationId": "ec33ebf9-0cba-4100-8142-c61503f6c587"}
	for k, v := range fields {
		payload[k] = v
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return parseEvent(strings.NewReader(string(body)), m, spec, func(string) (string, bool) { return "", false })
}

func TestAPerCallPromptIsABoundaryOnlyAtItsFirstCall(t *testing.T) {
	m := perCallManifest(t)
	e, err := perCallEvent(t, m, "PreInvocation", map[string]any{"invocationNum": 0, "initialNumSteps": 4})
	if err != nil || e.TurnRef != "4" || e.EventRef != "4:PreInvocation" || e.Fact != wire.HookPromptSubmitted {
		t.Fatalf("first call: %+v (%v)", e, err)
	}
	for _, fields := range []map[string]any{
		{"invocationNum": 1, "initialNumSteps": 6},
		{"invocationNum": "0", "initialNumSteps": 4}, // a string is not the vendor's number
		{"invocationNum": 0.5, "initialNumSteps": 4},
		{"initialNumSteps": 4},
	} {
		if _, err := perCallEvent(t, m, "PreInvocation", fields); !errors.Is(err, errHookSkip) {
			t.Errorf("%v: got %v, want a skipped callback", fields, err)
		}
	}
	if _, err := perCallEvent(t, m, "PreInvocation", map[string]any{"invocationNum": 0}); !errors.Is(err, errHookInput) {
		t.Errorf("a first call without its step count: %v", err)
	}
	spec, _ := m.event("PreInvocation")
	// A repeated gate field, and numbers in other spellings.
	for _, number := range []string{`0,"invocationNum":0`, `-0`, `1e0`, `0.0`} {
		body := `{"hookEventName":"PreInvocation","conversationId":"ec33ebf9-0cba-4100-8142-c61503f6c587","invocationNum":` + number + `,"initialNumSteps":4}`
		if _, err := parseEvent(strings.NewReader(body), m, spec, func(string) (string, bool) { return "", false }); !errors.Is(err, errHookSkip) {
			t.Errorf("invocationNum %s: %v", number, err)
		}
	}
}

func TestAPerCallStopOffersOnlyAtItsFirstEligibleEnd(t *testing.T) {
	m := perCallManifest(t)
	eligible := map[string]any{"executionNum": 0, "terminationReason": "NO_TOOL_CALL", "fullyIdle": true, "error": ""}
	for _, tc := range []struct {
		name   string
		change map[string]any
		active string
		suffix string
	}{
		{"first end", nil, "false", ":0"},
		{"after a continuation", map[string]any{"executionNum": 1}, "true", ":1"},
		{"tool call pending", map[string]any{"terminationReason": "max_steps_exceeded"}, "", ":0"},
		{"background work", map[string]any{"fullyIdle": false}, "", ":0"},
		{"an error", map[string]any{"error": "model unavailable"}, "", ":0"},
		{"no execution number", map[string]any{"executionNum": nil}, "", ""},
		{"execution number as text", map[string]any{"executionNum": "0"}, "", ""},
	} {
		fields := map[string]any{}
		for k, v := range eligible {
			fields[k] = v
		}
		for k, v := range tc.change {
			if v == nil {
				delete(fields, k)
			} else {
				fields[k] = v
			}
		}
		e, err := perCallEvent(t, m, "Stop", fields)
		if err != nil || e.stopActive != tc.active || e.refSuffix != tc.suffix || e.TurnRef != "" || e.EventRef != "" {
			t.Errorf("%s: %+v (%v)", tc.name, e, err)
		}
		// The hook gives the Stop its session's open turn.
		e.TurnRef = "4"
		spec, _ := m.event("Stop")
		e.setEventRef(spec)
		if e.EventRef != "4:Stop"+tc.suffix {
			t.Errorf("%s: event ref %q", tc.name, e.EventRef)
		}
	}
}

func TestTheOpenTurnBelongsToItsGeneration(t *testing.T) {
	dir := t.TempDir()
	leaf, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer leaf.Close()
	if got := readOpenTurn(leaf, "g1"); got != "" {
		t.Fatalf("no record: %q", got)
	}
	if err := writeRecord(leaf, openTurnFile, "g1 4\n"); err != nil {
		t.Fatal(err)
	}
	if got := readOpenTurn(leaf, "g1"); got != "4" {
		t.Errorf("own generation: %q", got)
	}
	if got := readOpenTurn(leaf, "g2"); got != "" {
		t.Errorf("another generation: %q", got)
	}
	for _, record := range []string{"g1 4", "g1 \n", "g1 4 5\n", "g1\n"} {
		if err := writeRecord(leaf, openTurnFile, record); err != nil {
			t.Fatal(err)
		}
		if got := readOpenTurn(leaf, "g1"); got != "" {
			t.Errorf("%q: %q", record, got)
		}
	}
}

func TestAContinueStopCarriesTheLineAsItsReason(t *testing.T) {
	m := perCallManifest(t)
	spec, _ := m.event("Stop")
	out, err := presentTurnEnd(spec, "[crosshall] pending: 12 13")
	if err != nil || string(out) != `{"decision":"continue","reason":"[crosshall] pending: 12 13"}`+"\n" {
		t.Fatalf("%s (%v)", out, err)
	}
	if out, err := presentTurnEnd(spec, ""); out != nil || err != nil {
		t.Fatalf("nothing pending: %s (%v)", out, err)
	}
}
