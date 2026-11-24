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
	owner.must("owner", "channel", "create", "checkout")
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
	owner.must("owner", "channel", "create", "checkout")
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
	runHook := func(ctx context.Context, event, payload, socketPath string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, binary, "hook", "claude", event)
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
	session := claudeSession{w: w, key: sessionID}
	db := w.db()
	type route struct{ state, socket, reason string }
	var lastRoute route
	await := func(event string, want, pending route, payload func(int) string) {
		t.Helper()
		deadline := time.Now().Add(fixtureEventTimeout)
		ctx, cancel := context.WithDeadline(t.Context(), deadline)
		defer cancel()
		// A hook can exit successfully when its callback budget expires.
		for attempt := 1; ; attempt++ {
			runHook(ctx, event, payload(attempt), want.socket)
			session.waitForConsumption(deadline)
			var got route
			err := db.QueryRowContext(ctx, `SELECT state, COALESCE(socket_path, ''), reason FROM publish_route WHERE session_ref = ?`, sessionID).
				Scan(&got.state, &got.socket, &got.reason)
			if err == nil {
				lastRoute = got
			}
			if err == nil && got.state == want.state && got.socket == want.socket && (want.reason == "" || got.reason == want.reason) {
				return
			}
			waiting := err == nil && got == pending || event == "SessionStart" && errors.Is(err, sql.ErrNoRows)
			counter := session.lastCounter()
			if !waiting || counter != event+" deadline" || !time.Now().Before(deadline) {
				t.Fatalf("%s after %d callbacks: last route %+v (%v), want %+v, last hook counter %q",
					event, attempt, lastRoute, err, want, counter)
			}
		}
	}

	start := fmt.Sprintf(`{"hook_event_name":"SessionStart","session_id":%q,"source":"startup","cwd":%q,"permission_mode":"default"}`,
		sessionID, repo)
	started := route{state: "invalidated", reason: "no socket at start; the next prompt attaches it"}
	await("SessionStart", started, route{state: "invalidated", reason: "no socket"}, func(int) string { return start })
	await("UserPromptSubmit", route{state: "active", socket: socket}, started, func(attempt int) string {
		return fmt.Sprintf(`{"hook_event_name":"UserPromptSubmit","session_id":%q,"prompt_id":"prompt-%d","permission_mode":"default"}`,
			sessionID, attempt)
	})
}
