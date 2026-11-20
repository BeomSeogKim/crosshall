//go:build e2e

package e2e

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/ident"
)

func TestInstalledPromptHookRecoversMissedEnrollment(t *testing.T) {
	for _, test := range []struct {
		name, adapter, turn, env string
		provisional              bool
	}{
		{name: "claude missing leaf", adapter: "claude", turn: "prompt_id", env: "CLAUDE_CODE_SESSION_ID"},
		{name: "claude provisional launch", adapter: "claude", turn: "prompt_id", env: "CLAUDE_CODE_SESSION_ID", provisional: true},
		{name: "codex provisional leaf", adapter: "codex", turn: "turn_id", env: "CODEX_THREAD_ID", provisional: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newSetupWorld(t)
			f.apply("", "setup", test.adapter, "--apply")
			repo := f.enrollmentRepo()
			pid := strconv.Itoa(os.Getpid())
			if test.adapter == "claude" {
				f.env = append(f.env, "CLAUDE_PID="+pid, "AI_AGENT=claude-code_2-1-327_harness", "CLAUDE_CODE_ENTRYPOINT=cli")
			}
			key := setupSessionPrefix + test.adapter
			env := append(slices.Clone(f.env), test.env+"="+key)
			prompt := func(name string) string {
				return fmt.Sprintf(`%q:%q,"prompt":"fixture private text"`, test.turn, name)
			}
			if test.provisional {
				if got := f.command(env, "", "join", "missing-channel"); got.exit == 0 {
					t.Fatal("join unexpectedly succeeded")
				}
			}
			pids := f.dispatcherPIDs()
			if len(pids) != 1 {
				t.Fatalf("want one fixture dispatcher, got %v", pids)
			}
			child, err := os.FindProcess(pids[0])
			if err != nil {
				t.Fatal(err)
			}
			pauseFixtureProcess(t, child)
			start := f.invokeHook(repo, test.adapter, "SessionStart", `"source":"startup"`)
			stage := f.enrollmentEvidence(test.adapter)
			resumeFixtureProcess(t, child)
			wantStage := "no registered leaf"
			if test.provisional {
				wantStage = "provisional leaf without principal"
			}
			if start != "" || !strings.Contains(stage, wantStage) {
				t.Fatalf("stalled start output %q, %s", start, stage)
			}
			if test.provisional && test.adapter == "claude" {
				writeInterruptedClaudeLaunch(t, f.world, key, pid)
			}
			if !test.provisional {
				if got := f.invokeHook(f.home, test.adapter, "UserPromptSubmit", prompt("unmapped")); got != "" {
					t.Fatalf("unmapped prompt returned %q", got)
				}
				if got := f.invokeHook(repo, test.adapter, "Stop", prompt("stop")+`,"stop_hook_active":false`); got != "" {
					t.Fatalf("unenrolled Stop returned %q", got)
				}
				if got := f.enrollmentEvidence(test.adapter); !strings.Contains(got, "no registered leaf") {
					t.Fatal(got)
				}
			}

			out := f.invokeHook(repo, test.adapter, "UserPromptSubmit", prompt("recovered"))
			var native struct {
				Output struct {
					Event   string `json:"hookEventName"`
					Context string `json:"additionalContext"`
				} `json:"hookSpecificOutput"`
			}
			if err := json.Unmarshal([]byte(out), &native); err != nil || native.Output.Event != "UserPromptSubmit" ||
				!strings.Contains(native.Output.Context, "Joined #setup-check as @"+test.adapter+"1") {
				t.Fatalf("prompt recovery output %q (%v), %s", out, err, f.enrollmentEvidence(test.adapter))
			}
			if got := f.invokeHook(repo, test.adapter, "UserPromptSubmit", prompt("recovered")); got != "" {
				t.Fatalf("replayed callback returned %q", got)
			}
			if got := f.invokeHook(repo, test.adapter, "UserPromptSubmit", prompt("ordinary")); got != "" {
				t.Fatalf("established session received a second welcome %q", got)
			}

			db := f.db()
			var bindings, launches, observations, routes int
			if err := db.QueryRow(`SELECT
				(SELECT COUNT(*) FROM binding WHERE principal_id = k.principal_id AND state = 'live'),
				(SELECT COUNT(*) FROM adapter_launch WHERE principal_id = k.principal_id AND launch_ref = 'session' AND verified = 0
					AND started_hook_at_ms = 0 AND json_extract(profile, '$.mode') = 'unverified'),
				(SELECT COUNT(*) FROM observation WHERE principal_id = k.principal_id AND event_ref = 'recovered:UserPromptSubmit'),
				(SELECT COUNT(*) FROM publish_route WHERE principal_id = k.principal_id)
				FROM principal_key k WHERE vendor = ? AND key = ?`, test.adapter, key).Scan(&bindings, &launches, &observations, &routes); err != nil ||
				bindings != 1 || launches != 1 || observations != 1 || routes != 0 {
				t.Fatalf("recovery wrote bindings %d, launches %d, observations %d, routes %d (%v)",
					bindings, launches, observations, routes, err)
			}
			if test.adapter == "claude" {
				c := claudeSession{w: f.world, key: key, pid: pid, repo: repo}
				c.fireAndRecord("UserPromptSubmit", "prompt_submitted", map[string]any{"prompt_id": "bad-cwd", "cwd": []int{2}})
				c.fireAndRecord("SessionEnd", "session_ended", map[string]any{"reason": "other"})
				var launch string
				if err := db.QueryRow(`SELECT o.launch_ref FROM observation o JOIN principal_key k ON k.principal_id = o.principal_id
					WHERE k.vendor = 'claude' AND k.key = ? AND o.kind = 'session_ended'`, key).Scan(&launch); err != nil || launch != "session" {
					t.Fatalf("unverified end did not use the recovered launch: %q (%v)", launch, err)
				}
				if test.provisional {
					var count int
					if err := db.QueryRow(`SELECT COUNT(*) FROM op WHERE op_key LIKE 'hook:claude:pid-%:SessionEnd:%'`).Scan(&count); err != nil || count == 0 {
						t.Fatalf("fixture end did not carry the earlier local PID label: %d (%v)", count, err)
					}
				}
			}
			if got := f.command(env, "", "leave"); got.exit != 0 {
				t.Fatalf("explicit leave: %s", got.stdout)
			}
			if got := f.invokeHook(repo, test.adapter, "UserPromptSubmit", prompt("after-leave")); got != "" {
				t.Fatalf("departed session received %q", got)
			}
			if got := f.enrollmentEvidence(test.adapter); !strings.Contains(got, "linked leaf without live binding") {
				t.Fatal(got)
			}
		})
	}
}

func TestInstalledPromptHookChecksStartupRepository(t *testing.T) {
	for _, name := range []string{"no start", "start outside git", "different repository", "same repository mapped later"} {
		t.Run(name, func(t *testing.T) {
			f := newSetupWorld(t)
			f.apply("", "setup", "claude", "--apply")
			mapped := f.enrollmentRepo()
			other := filepath.Join(f.home, "other")
			if err := os.MkdirAll(filepath.Join(other, ".git"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(other, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0600); err != nil {
				t.Fatal(err)
			}
			start, prompt := other, mapped
			if name == "start outside git" {
				start = f.home
			}
			if name != "no start" {
				out := f.invokeHook(start, "claude", "SessionStart", `"source":"startup"`)
				if !strings.Contains(out, "crosshall join") || strings.Contains(out, "Joined #") {
					t.Fatalf("unmapped startup output %q", out)
				}
			}
			wantJoin := name == "same repository mapped later"
			if wantJoin {
				f.owner().must("owner", "channel", "default", "setup-check", "--repo", other)
				prompt = filepath.Join(other, "subdirectory")
				if err := os.Mkdir(prompt, 0700); err != nil {
					t.Fatal(err)
				}
			}
			out := f.invokeHook(prompt, "claude", "UserPromptSubmit", `"prompt_id":"after-start"`)
			stage := f.enrollmentEvidence("claude")
			if wantJoin && (!strings.Contains(out, "Joined #setup-check as @claude1") || !strings.Contains(stage, "live binding")) ||
				!wantJoin && (out != "" || !strings.Contains(stage, "no registered leaf")) {
				t.Fatalf("want join %v; prompt output %q, %s", wantJoin, out, stage)
			}
		})
	}
}

func writeInterruptedClaudeLaunch(t *testing.T, w *world, key, pid string) {
	t.Helper()
	digest := (ident.Identity{Vendor: "claude", Key: key}).Digest()
	leaf := filepath.Join(w.rendezvous(), "m", digest[:16])
	gen, err := os.ReadFile(filepath.Join(leaf, "gen"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(leaf, fmt.Sprintf("launch-%s-%x", pid, sha256.Sum256([]byte(strings.TrimSpace(string(gen))))))
	started := time.Now().Add(-time.Second).UnixMilli()
	ref := sha256.Sum256([]byte(fmt.Sprintf("SessionStart:startup:%d", started)))
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%d %x\n", started, ref)), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".lock", nil, 0600); err != nil {
		t.Fatal(err)
	}
}
