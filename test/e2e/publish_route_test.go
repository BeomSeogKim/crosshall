//go:build e2e

package e2e

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/ident"
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
	if out, err := exec.CommandContext(t.Context(), "git", "init", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	owner.must("owner", "channel", "default", "checkout", "--repo", repo)
	owner.must("owner", "adapter", "set", "claude", "--enabled", "on")

	sessionID := "claude-e2e-route"
	digest := (ident.Identity{Vendor: "claude", Key: sessionID}).Digest()
	leaf := filepath.Join(w.rendezvous(), "m", digest[:16])
	pid := fmt.Sprintf("%d", os.Getpid())
	runHook := func(event, payload, socketPath string, blockOutput bool) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), fixtureEventTimeout)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, "hook", "claude", event)
		cmd.Dir = repo
		cmd.Stdin = strings.NewReader(payload)
		cmd.Env = append(w.baseEnv(),
			"CLAUDE_CODE_SESSION_ID="+sessionID,
			"CLAUDE_PID="+pid,
			"CLAUDE_CODE_ENTRYPOINT=cli",
			"AI_AGENT=claude-code_2-1-327_harness",
		)
		if socketPath != "" {
			cmd.Env = append(cmd.Env, "CLAUDE_CODE_MESSAGING_SOCKET="+socketPath)
		}
		if blockOutput {
			blockHookProcess(t, cmd)
			return
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("hook %s: %v\n%s", event, err, out)
		}
	}

	type route struct{ state, mode, socket string }
	db := w.db()
	awaitRoute := func(want route, wantWho string, fire func(int)) {
		t.Helper()
		deadline := time.Now().Add(fixtureEventTimeout)
		var last route
		var lastErr error
		var who outcome
		var checkedWho bool
		var queued []string
		attempt := 0
		for time.Now().Before(deadline) {
			lastErr = db.QueryRow(`SELECT state, mode, COALESCE(socket_path, '')
				FROM publish_route WHERE session_ref = ? AND adapter = 'claude'`, sessionID).Scan(&last.state, &last.mode, &last.socket)
			if lastErr == nil && last == want {
				who, checkedWho = peer.run("who"), true
				members, _ := who.obj["members"].([]any)
				for _, item := range members {
					member, _ := item.(map[string]any)
					if pub, ok := member["publish"].(map[string]any); who.exit == 0 && member["vendor"] == "claude" && ok && pub["state"] == wantWho {
						return
					}
				}
			} else if lastErr != nil && !errors.Is(lastErr, sql.ErrNoRows) {
				break
			} else {
				queued = fixtureQueuedPackets(t, leaf)
				if len(queued) == 0 {
					attempt++
					fire(attempt)
				}
			}
			time.Sleep(25 * time.Millisecond)
		}
		if !checkedWho {
			who = peer.run("who")
		}
		t.Fatalf("after %d callbacks wanted committed route %+v and who state %q, last route %+v (%v), queued packets %v\nwho exit %d, stdout %s, stderr %s",
			attempt, want, wantWho, last, lastErr, queued, who.exit, who.stdout, who.stderr)
	}

	socket := "/tmp/cc-socks/" + pid + ".sock"
	start := fmt.Sprintf(`{"hook_event_name":"SessionStart","session_id":%q,"source":"startup","cwd":%q,"permission_mode":"default"}`, sessionID, repo)
	runHook("SessionStart", start, socket, true)
	awaitRoute(route{state: "active", mode: "default", socket: socket}, "candidate", func(int) {
		runHook("SessionStart", start, socket, false)
	})

	// Mode change through real spool client invalidates on bypassPermissions.
	awaitRoute(route{state: "invalidated", mode: "bypassPermissions", socket: socket}, "off", func(attempt int) {
		payload := fmt.Sprintf(`{"hook_event_name":"UserPromptSubmit","session_id":%q,"prompt_id":%q,"permission_mode":"bypassPermissions"}`,
			sessionID, fmt.Sprintf("bypass-%d", attempt))
		runHook("UserPromptSubmit", payload, "", false)
	})
}

// #180: a session whose start ran before the vendor's socket existed gets
// its route from the next prompt, without a restart.
func TestPublishRouteTakesTheSocketFromTheNextPrompt(t *testing.T) {
	w := newWorld(t)
	owner := w.owner()
	owner.must("owner", "start")
	owner.must("owner", "channel", "create", "checkout", "--pool", "0")
	w.member().mustFixtureWrite("join", "checkout", "--as", "form")
	repo, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(t.Context(), "git", "init", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v\n%s", err, out)
	}
	owner.must("owner", "channel", "default", "checkout", "--repo", repo)
	owner.must("owner", "adapter", "set", "claude", "--enabled", "on")

	sessionID := "claude-e2e-late-socket"
	pid := fmt.Sprintf("%d", os.Getpid())
	socket := "/tmp/cc-socks/" + pid + ".sock"
	runHook := func(event, payload, socketPath string) {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), binary, "hook", "claude", event)
		cmd.Dir = repo
		cmd.Stdin = strings.NewReader(payload)
		cmd.Env = append(w.baseEnv(), "CLAUDE_CODE_SESSION_ID="+sessionID, "CLAUDE_PID="+pid,
			"CLAUDE_CODE_ENTRYPOINT=cli", "AI_AGENT=claude-code_2-1-327_harness")
		if socketPath != "" {
			cmd.Env = append(cmd.Env, "CLAUDE_CODE_MESSAGING_SOCKET="+socketPath)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("hook %s: %v\n%s", event, err, out)
		}
	}
	db := w.db()
	await := func(what string, wantState, wantSocket, wantReason string) {
		t.Helper()
		var state, got, reason string
		for deadline := time.Now().Add(fixtureEventTimeout); time.Now().Before(deadline); time.Sleep(25 * time.Millisecond) {
			err := db.QueryRow(`SELECT state, COALESCE(socket_path, ''), reason FROM publish_route WHERE session_ref = ?`, sessionID).
				Scan(&state, &got, &reason)
			if err == nil && state == wantState && got == wantSocket && (wantReason == "" || reason == wantReason) {
				return
			}
		}
		t.Fatalf("%s: route state %q socket %q reason %q, want %q %q %q", what, state, got, reason, wantState, wantSocket, wantReason)
	}

	runHook("SessionStart", fmt.Sprintf(`{"hook_event_name":"SessionStart","session_id":%q,"source":"startup","cwd":%q,"permission_mode":"default"}`,
		sessionID, repo), "")
	await("a start without the socket", "invalidated", "", "no socket at start; the next prompt attaches it")
	runHook("UserPromptSubmit", fmt.Sprintf(`{"hook_event_name":"UserPromptSubmit","session_id":%q,"prompt_id":"first","permission_mode":"default"}`,
		sessionID), socket)
	await("the next prompt with the socket", "active", socket, "")
}
