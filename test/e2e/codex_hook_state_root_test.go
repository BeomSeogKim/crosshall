//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSetupCodexPinsGeneratedHooksToTheirStateRoot(t *testing.T) {
	selected := newSetupWorld(t)
	selected.state = filepath.Join(selected.home, "owner's selected state")
	envWithState := func(root string) []string {
		env := slices.DeleteFunc(slices.Clone(selected.env), func(entry string) bool {
			return strings.HasPrefix(entry, "CROSSHALL_STATE_DIR=")
		})
		return append(env, "CROSSHALL_STATE_DIR="+root)
	}
	selected.env = envWithState(selected.state)
	cold := newWorld(t)
	coldOwner, selectedOwner := cold.owner(), selected.owner()
	repo := filepath.Join(selected.home, "shared repository")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, target := range []struct {
		owner   *participant
		channel string
	}{
		{coldOwner, "cold-room"},
		{selectedOwner, "selected-room"},
	} {
		target.owner.must("owner", "channel", "create", target.channel)
		target.owner.must("owner", "channel", "default", target.channel, "--repo", repo)
	}
	// Make an unpinned hook enter the wrong channel instead of going silent.
	coldOwner.must("owner", "adapter", "set", "codex", "--enabled", "on")
	selected.apply("", "setup", "codex", "--apply")

	invoke := func(env []string, event, fields string) string {
		t.Helper()
		hook := setupHookFor(t, readSetupFile(t, selected.paths["codex"]), event)
		if len(hook.Args) != 0 || hook.Timeout != 1 || !strings.HasSuffix(hook.Command, " hook codex "+event) {
			t.Fatalf("setup changed the native hook invocation: %+v", hook)
		}
		payload := fmt.Sprintf(`{"hook_event_name":%q,"session_id":"synthetic-shared-daemon","cwd":%q,%s}`, event, repo, fields)
		cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", hook.Command)
		cmd.Dir, cmd.Env, cmd.Stdin = repo, env, strings.NewReader(payload)
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil || stderr.Len() != 0 {
			t.Fatalf("installed Codex %s hook: %v, stderr %q", event, err, stderr.String())
		}
		if !strings.Contains(hook.Command, "CROSSHALL_STATE_DIR=") {
			t.Errorf("installed Codex %s command omitted its state assignment", event)
		}
		return hostContext(t, stdout.String())
	}

	// Run the actual command from hooks.json in a shell carrying the cold
	// daemon's state directory. The inline assignment must select its own root.
	coldEnv := envWithState(cold.state)
	if got := invoke(coldEnv, "SessionStart", `"source":"startup"`); !strings.Contains(got, "Joined #selected-room as @codex1") {
		t.Errorf("hook under a cold-root environment did not join the configured default: %q", got)
	}
	coldChannel := soleLedgerRow(t, coldOwner.must("owner", "who", "--all").obj, "channels")
	if rows, ok := coldChannel["members"].([]any); !ok || len(rows) != 0 {
		t.Errorf("the cold root gained a binding: %v", coldChannel)
	}
	selectedChannel := soleLedgerRow(t, selectedOwner.must("owner", "who", "--all").obj, "channels")
	if rows, ok := selectedChannel["members"].([]any); !ok || len(rows) != 1 {
		t.Errorf("the configured root did not gain one binding: %v", selectedChannel)
	}
	if t.Failed() {
		return
	}

	writer := selected.member()
	writer.must("join", "selected-room", "--as", "writer")
	for i, env := range [][]string{coldEnv, selected.env} {
		ids := notes(t, writer, "codex1", 1)
		fields := fmt.Sprintf(`"turn_id":"synthetic-turn-%d"`, i+1)
		if got := invoke(env, "UserPromptSubmit", fields); got != pendingContext(ids) {
			t.Errorf("installed prompt %d returned %q, want %q", i+1, got, pendingContext(ids))
		}
	}
}
