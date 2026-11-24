//go:build conformance

package conformance

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// Match only crosshall's rendered text, excluding host wrappers, conversation
// history outside the sample and whole tool-result bodies.
var tokenSamplePatterns = map[string]*regexp.Regexp{
	"hint":     regexp.MustCompile(`\[crosshall\] Joined #[^ \n]+ as @[^ \n]+, with @[^\n]+`),
	"pending":  regexp.MustCompile(`\[crosshall\] pending: [0-9]+(?: [0-9]+)*`),
	"reminder": regexp.MustCompile(`\[crosshall\] you picked up [0-9]+(?: [0-9]+)* and have not replied\. Reply, decline, or ask for more time\.`),
	"presence": regexp.MustCompile(`\[crosshall\] @[a-z0-9-]+ (?:prompt submitted|turn finished|approval requested|session ended) [0-9]+[a-z]+ ago(?: \(unverified\))?`),
}

// logRequestTokenSamples emits individual text samples for the offline counter
// only when requested. A missing or duplicated sample fails the measurement.
func logRequestTokenSamples(t *testing.T, vendor string, body []byte, wanted ...string) {
	t.Helper()
	if os.Getenv("CROSSHALL_MEASURE_TEXT") != "1" {
		return
	}
	var request struct {
		Messages []json.RawMessage
		Input    []json.RawMessage
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Error(err)
		return
	}
	found := make(map[string]int)
	visit := func(role, text string) {
		for _, kind := range wanted {
			for _, sample := range tokenSamplePatterns[kind].FindAllString(text, -1) {
				found[kind]++
				row, _ := json.Marshal(map[string]string{"vendor": vendor, "kind": kind, "role": role, "text": sample})
				t.Logf("TOKEN_SAMPLE %s", row)
			}
		}
	}
	var walk func(json.RawMessage, string)
	walk = func(raw json.RawMessage, role string) {
		switch {
		case len(raw) == 0:
		case raw[0] == '"':
			var s string
			if json.Unmarshal(raw, &s) == nil {
				visit(role, s)
			}
		case raw[0] == '[':
			var parts []json.RawMessage
			_ = json.Unmarshal(raw, &parts)
			for _, part := range parts {
				walk(part, role)
			}
		case raw[0] == '{':
			var part struct {
				Type, Role, Text string
				Content, Output  json.RawMessage
			}
			_ = json.Unmarshal(raw, &part)
			if part.Role != "" {
				role = part.Role
			}
			switch part.Type {
			case "function_call_output":
				walk(part.Output, "tool")
			case "tool_result":
				walk(part.Content, "tool")
			default:
				if part.Text != "" {
					visit(role, part.Text)
				}
				walk(part.Content, role)
			}
		}
	}
	for _, raw := range append(request.Messages, request.Input...) {
		walk(raw, "")
	}
	for _, kind := range wanted {
		if found[kind] != 1 {
			t.Errorf("%s %s: expected one model-visible sample, got %d", vendor, kind, found[kind])
		}
	}
}

// Both the host continuation and the fixture request are real. The Stop text
// is controlled here so this measurement cannot arm or certify the reminder.
func TestClaudeReminderTokenCost(t *testing.T) {
	if os.Getenv("CROSSHALL_MEASURE_TEXT") != "1" {
		t.Skip("set CROSSHALL_MEASURE_TEXT=1 for the token measurement")
	}
	c := newConformance(t)
	c.requireClaude()
	const reason = "[crosshall] you picked up 1 and have not replied. Reply, decline, or ask for more time."
	payload, err := json.Marshal(map[string]any{"hookSpecificOutput": map[string]string{"hookEventName": "Stop", "additionalContext": reason}})
	if err != nil {
		t.Fatal(err)
	}
	// mkdir succeeds only on the first Stop in this throwaway home.
	path := c.root + "/measure-stop.sh"
	script := fmt.Sprintf("#!/bin/sh\ncat >/dev/null\nmkdir %s 2>/dev/null || exit 0\nprintf '%%s\\n' %s\n", codexQuote(c.root+"/used"), codexQuote(string(payload)))
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	settings, _ := json.Marshal(map[string]any{"hooks": map[string]any{"Stop": []any{
		map[string]any{"hooks": []any{map[string]any{"type": "command", "command": path, "timeout": 1}}},
	}}})
	if err := os.WriteFile(c.config+"/settings.json", settings, 0600); err != nil {
		t.Fatal(err)
	}
	// In a real reminder turn a tool ran before Stop. Keep its result in
	// the transcript so the measured continuation has that same history.
	const command = "printf 'fixture-%s' pickup"
	c.model.bash[1] = command
	c.claudeRun("", "--tools", "Bash", "--allowedTools", "Bash("+command+")", "--", "Complete the fixture turn.")
	if c.model.count() != 3 || strings.Contains(c.model.request(2), reason) {
		t.Fatalf("expected exactly one Stop continuation, got %d requests", c.model.count())
	}
	if ok, detail := returnedMarker([]byte(c.model.request(2)), "toolu_fixture01", "fixture-pickup"); !ok {
		t.Fatalf("missing the completed Bash result before Stop: %s", detail)
	}
	var envelope struct {
		Messages []struct {
			Role    string
			Content json.RawMessage
		}
		System json.RawMessage
	}
	if err := json.Unmarshal([]byte(c.model.request(3)), &envelope); err != nil {
		t.Fatal(err)
	}
	for _, msg := range envelope.Messages {
		if i := strings.Index(string(msg.Content), reason); i >= 0 {
			t.Logf("outer Claude Messages role=%s wrapper=%s", msg.Role, string(msg.Content)[max(0, i-100):i])
		}
	}
	if strings.Contains(string(envelope.System), reason) {
		t.Fatal("reminder entered the system prompt")
	}
	logRequestTokenSamples(t, "claude", []byte(c.model.request(3)), "reminder")
}

// A peer's prompt arrives after the enrollment roster and before the ordinary
// CLI command. Count the coalesced change in the tool's real return value.
func TestClaudePresenceTokenCost(t *testing.T) {
	if os.Getenv("CROSSHALL_MEASURE_TEXT") != "1" {
		t.Skip("set CROSSHALL_MEASURE_TEXT=1 for the token measurement")
	}
	c := newConformance(t)
	c.requireClaude()
	c.owner("owner", "start")
	c.owner("owner", "channel", "create", "checkout")
	c.owner("owner", "channel", "default", "checkout", "--repo", c.repo)
	c.owner("owner", "adapter", "set", "claude", "--enabled", "on")
	c.owner("owner", "adapter", "set", "codex", "--enabled", "on")
	c.install()
	peerHook := func(event string, fields map[string]string) error {
		fields["hook_event_name"], fields["session_id"], fields["cwd"] = event, "presence-fixture-peer", c.repo
		data, err := json.Marshal(fields)
		if err != nil {
			return err
		}
		cmd := exec.Command(c.crosshall, "hook", "codex", event)
		cmd.Env, cmd.Dir = c.base(), c.repo
		cmd.Stdin = strings.NewReader(string(data))
		return cmd.Run()
	}
	if err := peerHook("SessionStart", map[string]string{"source": "startup"}); err != nil {
		t.Fatal(err)
	}
	c.model.during[1] = func() {
		if err := peerHook("UserPromptSubmit", map[string]string{"turn_id": "cost-turn"}); err != nil {
			t.Error(err)
		}
	}
	command := c.crosshall + " inbox"
	c.model.bash[1] = command
	c.claudeRun("", "--tools", "Bash", "--allowedTools", "Bash("+command+")", "--", "Check the inbox.")
	if c.model.count() != 2 {
		t.Fatalf("expected one tool result, got %d model requests", c.model.count())
	}
	logRequestTokenSamples(t, "claude", []byte(c.model.request(2)), "presence")
}
