//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestAntigravitySetupInstallsAndInvokesOnlyCLIStartup(t *testing.T) {
	f := newSetupWorld(t)
	vendor := filepath.Join(filepath.Dir(f.executable), "agy")
	if err := os.WriteFile(vendor, []byte("#!/bin/sh\n[ \"$1\" = --version ] || exit 9\nprintf '1.5.13\\n'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(f.home, ".gemini", "config", "hooks.json")
	if err := os.MkdirAll(filepath.Dir(config), 0700); err != nil {
		t.Fatal(err)
	}
	const userHooks = "{\n  \"custom\": {\"enabled\":false,\"SessionStart\":[{\"type\":\"command\",\"command\":\"keep-user-hook\"}]}\n}\n"
	if err := os.WriteFile(config, []byte(userHooks), 0600); err != nil {
		t.Fatal(err)
	}
	original := readSetupFile(t, config)
	f.apply("", "setup", "antigravity", "--apply")
	if result := f.command(f.env, "", "owner", "adapter", "set", "agy", "--publish", "on"); result.exit != 2 ||
		!strings.Contains(result.stdout, "Antigravity has no publish adapter") {
		t.Fatalf("publish policy for Antigravity: %+v", result)
	}
	diagnostic := f.apply("", "owner", "doctor")
	var doctor struct {
		Adapters []struct {
			Adapter string
			Publish struct{ State string }
		}
	}
	if err := json.Unmarshal([]byte(diagnostic.stdout), &doctor); err != nil {
		t.Fatal(err)
	}
	var publishState string
	for _, adapter := range doctor.Adapters {
		if adapter.Adapter == "agy" {
			publishState = adapter.Publish.State
		}
	}
	if publishState != "off" {
		t.Fatalf("Antigravity publish state = %q, want off", publishState)
	}
	var installed map[string]map[string]json.RawMessage
	if err := json.Unmarshal(readSetupFile(t, config).bytes, &installed); err != nil {
		t.Fatal(err)
	}
	var hooks []setupHook
	if err := json.Unmarshal(installed["crosshall"]["SessionStart"], &hooks); err != nil || len(hooks) != 1 ||
		len(installed["crosshall"]) != 1 || hooks[0].Timeout != 1 || len(hooks[0].Args) != 0 {
		t.Fatalf("installed hooks: %s, error %v", installed["crosshall"], err)
	}
	repo := f.enrollmentRepo()
	const conversation = "ec33ebf9-0cba-4100-8142-c61503f6c587"
	invoke := func(surface string) string {
		t.Helper()
		payload, err := json.Marshal(map[string]any{
			"conversationId": conversation,
			"workspacePaths": []string{repo},
			"transcriptPath": filepath.Join(f.home, ".gemini", surface, "brain", conversation,
				".system_generated", "logs", "transcript_full.jsonl"),
			"lastUserInput": "private input",
		})
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", hooks[0].Command)
		cmd.Dir, cmd.Env = repo, append(f.env, "CROSSHALL_STATE_DIR="+filepath.Join(f.home, "other state"))
		cmd.Stdin = strings.NewReader(string(payload))
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("execute installed hook: %v, output %q", err, out)
		}
		return string(out)
	}
	if got := invoke("antigravity"); got != "{}\n" {
		t.Fatalf("non-CLI payload should be a no-op: %q", got)
	}
	before, err := f.hookCounterRecords("agy", "SessionStart")
	if err != nil {
		t.Fatal(err)
	}
	out := invoke("antigravity-cli")
	if out == "{}\n" {
		after, err := f.hookCounterRecords("agy", "SessionStart")
		if err != nil {
			t.Fatal(err)
		}
		if len(after) == len(before)+1 && strings.HasPrefix(after[len(before)], "agy SessionStart deadline ") {
			out = invoke("antigravity-cli")
		}
	}
	if !strings.Contains(out, `"injectSteps":[{"systemMessage":{"systemMessage":`) ||
		!strings.Contains(out, "Joined #setup-check as @agy1") || strings.Contains(out, "private input") {
		f.logDispatcher()
		t.Fatalf("wrong installed SessionStart output: %q (last hook counter %q)", out, f.hookCounter("agy", "SessionStart"))
	}
	var routes int
	if err := f.db().QueryRow("SELECT COUNT(*) FROM publish_route WHERE adapter = 'agy'").Scan(&routes); err != nil || routes != 0 {
		t.Fatalf("Antigravity startup acquired %d publish routes: %v", routes, err)
	}
	installedFile := readSetupFile(t, config)
	for _, args := range [][]string{{"uninstall", "antigravity"}, {"uninstall"}} {
		preview := f.apply("yes\n", args...)
		if preview.obj["dry_run"] != true || preview.obj["applied"] == true {
			t.Fatalf("redirected %v did not preview: %s", args, preview.stdout)
		}
		assertSetupFile(t, config, installedFile, true)
	}
	f.apply("", "uninstall", "antigravity", "--apply")
	assertSetupFile(t, config, original, false)
	if got := invoke("antigravity-cli"); got != "{}\n" {
		t.Fatalf("uninstalled hook still has an effect: %q", got)
	}
}
