//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

const agyConversation = "0b6e4f6a-7d1c-4a8e-9f21-3c5d2e1f0a9b"

// newAgyWorld installs the Antigravity hooks for a fake agy executable.
func newAgyWorld(t *testing.T) (*setupWorld, map[string]string) {
	t.Helper()
	f := newSetupWorld(t)
	vendor := filepath.Join(filepath.Dir(f.executable), "agy")
	if err := os.WriteFile(vendor, []byte("#!/bin/sh\n[ \"$1\" = --version ] || exit 9\nprintf '1.5.16\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(f.home, ".gemini", "config", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(config), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(config, []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	f.apply("", "setup", "antigravity", "--apply")
	var installed map[string]map[string][]setupHook
	if err := json.Unmarshal(readSetupFile(t, config).bytes, &installed); err != nil {
		t.Fatal(err)
	}
	commands := map[string]string{}
	for event, hooks := range installed["crosshall"] {
		if len(hooks) != 1 {
			t.Fatalf("%s hooks: %+v", event, hooks)
		}
		commands[event] = hooks[0].Command
	}
	if len(commands) != 3 || commands["PreInvocation"] == "" || commands["Stop"] == "" {
		t.Fatalf("installed Antigravity hooks: %v", commands)
	}
	return f, commands
}

// invokeAgy runs an installed Antigravity hook with the payload fields the
// CLI sends (#328 pilot), plus fields.
func (f *setupWorld) invokeAgy(command, repo, event string, fields map[string]any) string {
	f.t.Helper()
	payload := map[string]any{
		"hookEventName":  event,
		"conversationId": agyConversation,
		"workspacePaths": []string{repo},
		"transcriptPath": filepath.Join(f.home, ".gemini", "antigravity-cli", "brain", agyConversation,
			".system_generated", "logs", "transcript.jsonl"),
		"lastUserInput": "private input",
	}
	for k, v := range fields {
		payload[k] = v
	}
	body, err := json.Marshal(payload)
	if err != nil {
		f.t.Fatal(err)
	}
	cmd := exec.CommandContext(f.t.Context(), "/bin/sh", "-c", command)
	cmd.Dir, cmd.Env, cmd.Stdin = repo, f.env, strings.NewReader(string(body))
	out, err := cmd.CombinedOutput()
	if err != nil {
		f.t.Fatalf("invoke %s: %v, output %q", event, err, out)
	}
	if strings.Contains(string(out), "private input") {
		f.t.Fatalf("%s echoed the prompt: %q", event, out)
	}
	return strings.TrimSpace(string(out))
}

// injected is the text of an injectSteps answer, "" for "{}".
func injected(t *testing.T, out string) string {
	t.Helper()
	if out == "{}" {
		return ""
	}
	var v struct {
		Steps []struct {
			SystemMessage struct {
				Text string `json:"systemMessage"`
			} `json:"systemMessage"`
		} `json:"injectSteps"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil || len(v.Steps) != 1 {
		t.Fatalf("not one injected step: %q (%v)", out, err)
	}
	return v.Steps[0].SystemMessage.Text
}

// agy-2: a request reaches an Antigravity session at the first model call
// of its next prompt, never at a later call of the same turn, and a Stop
// without turn-end evidence only observes.
func TestAntigravityGetsARequestAtItsNextPromptsFirstModelCall(t *testing.T) {
	f, commands := newAgyWorld(t)
	repo := f.enrollmentRepo()
	deadline := time.Now().Add(fixtureEventTimeout)
	for {
		out := f.invokeAgy(commands["SessionStart"], repo, "SessionStart", nil)
		if strings.Contains(injected(t, out), "Joined #setup-check as @agy1") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no welcome: %q; %s", out, f.enrollmentEvidence("agy"))
		}
	}
	writer := f.member()
	writer.must("join", "setup-check", "--as", "writer")
	id := strconv.FormatInt(num(t, writer.must("ask", "@agy1", "a request for Antigravity"), "id"), 10)
	want := pendingContext([]string{id})

	prompt := func(call, steps int) string {
		return injected(t, f.invokeAgy(commands["PreInvocation"], repo, "PreInvocation",
			map[string]any{"invocationNum": call, "initialNumSteps": steps}))
	}
	if got := prompt(1, 2); got != "" {
		t.Fatalf("a later model call offered %q", got)
	}
	deadline = time.Now().Add(fixtureEventTimeout)
	for got := prompt(0, 3); got != want; got = prompt(0, 3) {
		if got != "" || time.Now().After(deadline) {
			t.Fatalf("the first model call offered %q, want %q", got, want)
		}
	}
	if got := prompt(1, 5); got != "" {
		t.Fatalf("the turn's next model call offered %q again", got)
	}
	stop := f.invokeAgy(commands["Stop"], repo, "Stop",
		map[string]any{"executionNum": 0, "terminationReason": "NO_TOOL_CALL", "fullyIdle": true, "error": ""})
	if stop != "{}" {
		t.Fatalf("a Stop without turn-end evidence answered %q", stop)
	}

	member := &participant{w: f.world, extra: []string{"ANTIGRAVITY_CONVERSATION_ID=" + agyConversation, "ANTIGRAVITY_LS_VERSION=cli-1.5.16"}}
	member.must("read")
	member.must("reply", id, "done")
	if got := writer.must("read").stdout; !strings.Contains(got, "done") {
		t.Fatalf("the writer did not get the reply: %s", got)
	}

	f.owner().must("owner", "adapter", "set", "agy", "--enabled", "off")
	writer.must("ask", "@agy1", "while policy is off")
	if got := prompt(0, 9); got != "" {
		t.Fatalf("a disabled adapter's prompt offered %q", got)
	}
}
