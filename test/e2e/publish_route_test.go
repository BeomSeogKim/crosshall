//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPublishRouteThroughRealSpoolClient(t *testing.T) {
	w := newWorld(t)
	owner := w.owner()
	owner.must("owner", "start")
	owner.must("owner", "channel", "create", "checkout", "--pool", "0")
	peer := w.member()
	peer.mustFixtureWrite("join", "checkout", "--as", "form")
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	owner.must("owner", "channel", "default", "checkout", "--repo", repo)
	owner.must("owner", "adapter", "set", "claude", "--enabled", "on")

	sessionID := "claude-e2e-route"
	pid := fmt.Sprintf("%d", os.Getpid())
	runHook := func(event, payload, socketPath string) {
		t.Helper()
		cmd := exec.Command(binary, "hook", "claude", event)
		cmd.Dir = repo
		cmd.Stdin = strings.NewReader(payload)
		var env []string
		for _, kv := range os.Environ() {
			if strings.HasPrefix(kv, "ANTIGRAVITY_") || strings.HasPrefix(kv, "CLAUDE_") ||
				strings.HasPrefix(kv, "CODEX_") || strings.HasPrefix(kv, "CROSSHALL_") ||
				strings.HasPrefix(kv, "AI_AGENT=") {
				continue
			}
			env = append(env, kv)
		}
		env = append(env,
			"CROSSHALL_STATE_DIR="+w.state,
			w.dispatcherLeaseEnv(),
			"CLAUDE_CODE_SESSION_ID="+sessionID,
			"CLAUDE_PID="+pid,
			"CLAUDE_CODE_ENTRYPOINT=cli",
			"AI_AGENT=claude-code_2-1-327_harness",
		)
		if socketPath != "" {
			env = append(env, "CLAUDE_CODE_MESSAGING_SOCKET="+socketPath)
		}
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("hook %s: %v\n%s", event, err, out)
		}
	}

	runHook("SessionStart",
		`{"hook_event_name":"SessionStart","session_id":"`+sessionID+`","source":"startup","cwd":"`+repo+`","permission_mode":"default"}`,
		"/tmp/cc-socks/"+pid+".sock")

	waitWhoPublish := func(wantState string) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			who := peer.must("who")
			raw, _ := who.obj["members"].([]any)
			for _, item := range raw {
				m, _ := item.(map[string]any)
				if m["vendor"] == "claude" {
					if pub, ok := m["publish"].(map[string]any); ok && pub["state"] == wantState {
						return
					}
				}
			}
			time.Sleep(25 * time.Millisecond)
		}
		t.Fatalf("did not observe claude publish.state == %q in who", wantState)
	}

	waitWhoPublish("candidate")

	// Mode change through real spool client invalidates on bypassPermissions.
	runHook("UserPromptSubmit",
		`{"hook_event_name":"UserPromptSubmit","session_id":"`+sessionID+`","prompt_id":"p1","permission_mode":"bypassPermissions"}`,
		"")
	waitWhoPublish("off")
}
