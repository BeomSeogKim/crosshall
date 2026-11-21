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
			if out := f.waitForStartupRepository(repo, test.adapter, "startup", repo); out != "" {
				t.Fatalf("stalled startup emitted %q", out)
			}
			stage := f.enrollmentEvidence(test.adapter)
			resumeFixtureProcess(t, child)
			wantStage := "no registered leaf"
			if test.provisional {
				wantStage = "provisional leaf without principal"
			}
			if !strings.Contains(stage, wantStage) {
				t.Fatalf("unexpected enrollment after stalled start: %s", stage)
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
				pauseFixtureProcess(t, child)
				missed := f.invokeHook(repo, test.adapter, "UserPromptSubmit", prompt("dispatcher-still-paused"))
				missedStage := f.enrollmentEvidence(test.adapter)
				resumeFixtureProcess(t, child)
				if missed != "" || !strings.Contains(missedStage, "no registered leaf") {
					t.Fatalf("first recovery unexpectedly enrolled while paused: %q, %s", missed, missedStage)
				}
			}

			recovered := f.waitForEnrollmentWelcome(repo, test.adapter, "UserPromptSubmit", prompt)
			if got := f.invokeHook(repo, test.adapter, "UserPromptSubmit", prompt(recovered)); got != "" {
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
				(SELECT COUNT(*) FROM observation WHERE principal_id = k.principal_id AND event_ref = ?),
				(SELECT COUNT(*) FROM publish_route WHERE principal_id = k.principal_id)
				FROM principal_key k WHERE vendor = ? AND key = ?`, recovered+":UserPromptSubmit", test.adapter, key).Scan(&bindings, &launches, &observations, &routes); err != nil ||
				bindings != 1 || launches != 1 || observations != 1 || routes != 0 {
				t.Fatalf("recovery wrote bindings %d, launches %d, observations %d, routes %d (%v)",
					bindings, launches, observations, routes, err)
			}
			if test.adapter == "claude" {
				c := claudeSession{w: f.world, key: key, pid: pid, repo: repo}
				if !test.provisional {
					// An interrupted hook can leave a staging file without publishing a packet.
					digest := (ident.Identity{Vendor: "claude", Key: key}).Digest()
					part := filepath.Join(f.rendezvous(), "m", digest[:16], "in", strings.Repeat("a", 32)+".part")
					if err := os.WriteFile(part, nil, 0600); err != nil {
						t.Fatal(err)
					}
				}
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

func (f *setupWorld) waitForStartupRepository(cwd, adapter, source, repo string) string {
	f.t.Helper()
	identity := ident.Identity{Vendor: adapter, Key: setupSessionPrefix + adapter}
	path := filepath.Join(f.state, "hook-start-repos", identity.Digest())
	want := "-\n"
	if repo != "" {
		want = fmt.Sprintf("%x\n", sha256.Sum256([]byte(repo)))
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		out := f.invokeHook(cwd, adapter, "SessionStart", fmt.Sprintf(`"source":%q`, source))
		if strings.Contains(out, "Joined #") {
			f.t.Fatalf("startup enrolled before the fixture was ready to recover: %q", out)
		}
		record, err := os.ReadFile(path)
		if err == nil && string(record) == want {
			return out
		}
		if !os.IsNotExist(err) || !time.Now().Before(deadline) {
			f.t.Fatalf("startup repository record %q (%v), want %q; %s", record, err, want, f.enrollmentEvidence(adapter))
		}
	}
}

func (f *setupWorld) waitForEnrollmentWelcome(cwd, adapter, event string, fields func(string) string) string {
	f.t.Helper()
	db := f.db()
	deadline := time.Now().Add(5 * time.Second)
	var delivered string
	for attempt := 1; time.Now().Before(deadline); attempt++ {
		turn := fmt.Sprintf("recovery-%d", attempt)
		out := f.invokeHook(cwd, adapter, event, fields(turn))
		if out != "" {
			var native struct {
				Output struct {
					Event   string `json:"hookEventName"`
					Context string `json:"additionalContext"`
				} `json:"hookSpecificOutput"`
			}
			if err := json.Unmarshal([]byte(out), &native); err != nil || native.Output.Event != event {
				f.t.Fatalf("unexpected %s response %q (%v), %s", event, out, err, f.enrollmentEvidence(adapter))
			}
			if strings.Contains(native.Output.Context, "Joined #setup-check as @"+adapter+"1") {
				delivered = turn
			} else if delivered == "" || event != "SessionStart" {
				f.t.Fatalf("%s did not deliver the welcome: %q; %s", event, out, f.enrollmentEvidence(adapter))
			}
		}
		if delivered != "" {
			if _, welcome := hookJoinAcknowledgements(f.t, db, adapter); welcome {
				return delivered
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	f.t.Fatalf("no acknowledged recovery welcome (last delivered attempt %q, %s counter %q); %s",
		delivered, event, f.hookCounter(adapter, event), f.enrollmentEvidence(adapter))
	return ""
}

func recoveryPrompt(turn string) string {
	return fmt.Sprintf(`"prompt_id":%q`, turn)
}

func TestInstalledPromptHookChecksStartupRepository(t *testing.T) {
	for _, name := range []string{"no start", "start outside git", "different repository", "same repository mapped later"} {
		t.Run(name, func(t *testing.T) {
			f := newSetupWorld(t)
			f.apply("", "setup", "claude", "--apply")
			f.owner().must("owner", "channel", "auto", "off") // a start with no channel is told how to join
			mapped := f.enrollmentRepo()
			other := filepath.Join(f.home, "other")
			if err := os.MkdirAll(filepath.Join(other, ".git"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(other, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0600); err != nil {
				t.Fatal(err)
			}
			start, prompt := other, mapped
			startRepo := other
			if name == "start outside git" {
				start = f.home
				startRepo = ""
			}
			if name != "no start" {
				out := f.waitForStartupRepository(start, "claude", "startup", startRepo)
				if out != "" && !strings.Contains(out, "crosshall join") {
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
				f.waitForEnrollmentWelcome(prompt, "claude", "UserPromptSubmit", recoveryPrompt)
				return
			}
			out := f.invokeHook(prompt, "claude", "UserPromptSubmit", `"prompt_id":"after-start"`)
			if stage := f.enrollmentEvidence("claude"); out != "" || !strings.Contains(stage, "no registered leaf") {
				t.Fatalf("unmatched prompt output %q, %s", out, stage)
			}
		})
	}
}

func TestInstalledHookRecordsOtherStartSources(t *testing.T) {
	for _, source := range []string{"resume", "clear", "compact"} {
		t.Run(source, func(t *testing.T) {
			f := newSetupWorld(t)
			f.apply("", "setup", "claude", "--apply")
			f.owner().must("owner", "channel", "create", "setup-check")
			f.member().must("join", "setup-check", "--as", "peer")
			repo := filepath.Join(f.home, "later-mapped")
			if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if got := f.waitForStartupRepository(repo, "claude", source, repo); got != "" {
				t.Fatalf("unmapped %s produced %q", source, got)
			}
			f.owner().must("owner", "channel", "default", "setup-check", "--repo", repo)
			f.waitForEnrollmentWelcome(repo, "claude", "UserPromptSubmit", recoveryPrompt)
		})
	}
}

// A later start follows the rule of a prompt: it enrols a session only in
// the repository of its first start. A start after the session ended is a
// first start again.
func TestInstalledLaterStartChecksStartupRepository(t *testing.T) {
	for _, source := range []string{"resume", "clear", "compact"} {
		for _, place := range []string{"outside git", "different repository", "same repository mapped later", "ended first"} {
			t.Run(source+" "+place, func(t *testing.T) {
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
				start, later := other, mapped
				startRepo := other
				if place == "outside git" {
					start = f.home
					startRepo = ""
				}
				f.waitForStartupRepository(start, "claude", "startup", startRepo)
				wantJoin := false
				switch place {
				case "same repository mapped later":
					f.owner().must("owner", "channel", "default", "setup-check", "--repo", other)
					later, wantJoin = other, true
				case "ended first":
					f.invokeHook(start, "claude", "SessionEnd", `"reason":"other"`)
					wantJoin = true
				}
				fields := fmt.Sprintf(`"source":%q`, source)
				if wantJoin {
					f.waitForEnrollmentWelcome(later, "claude", "SessionStart", func(string) string { return fields })
					return
				}
				out := f.invokeHook(later, "claude", "SessionStart", fields)
				if stage := f.enrollmentEvidence("claude"); out != "" || !strings.Contains(stage, "no registered leaf") {
					t.Fatalf("unmatched %s start output %q, %s", source, out, stage)
				}
			})
		}
	}
}

func TestInstalledHookKeepsAndPrunesStartupRecord(t *testing.T) {
	for _, action := range []string{"join on start", "join after owner restart", "join by command", "end while owner stopped"} {
		t.Run(action, func(t *testing.T) {
			f := newSetupWorld(t)
			f.apply("", "setup", "claude", "--apply")
			repo := f.enrollmentRepo()
			key := setupSessionPrefix + "claude"
			id := ident.Identity{Vendor: "claude", Key: key}
			marker := filepath.Join(f.state, "hook-start-repos", id.Digest())
			owner := f.owner()
			if action == "join after owner restart" || action == "end while owner stopped" {
				owner.must("owner", "stop")
			}
			start := repo
			if action == "join by command" {
				start = f.home
			}
			out := f.invokeHook(start, "claude", "SessionStart", `"source":"startup"`)
			if action == "join on start" {
				if !strings.Contains(out, "Joined #setup-check as @claude1") {
					t.Fatalf("mapped startup produced %q", out)
				}
			} else {
				if strings.Contains(out, "Joined #") {
					t.Fatalf("startup enrolled early: %q", out)
				}
				if _, err := os.Stat(marker); err != nil {
					t.Fatalf("startup record missing: %v", err)
				}
			}
			if action == "join after owner restart" || action == "end while owner stopped" {
				if pids := f.dispatcherPIDs(); len(pids) != 0 {
					t.Fatalf("start hook restarted the dispatcher: %v", pids)
				}
			}
			switch action {
			case "join after owner restart":
				owner.must("owner", "start")
				if got := f.invokeHook(repo, "claude", "UserPromptSubmit", `"prompt_id":"after-restart"`); !strings.Contains(got, "Joined #setup-check as @claude1") {
					t.Fatalf("prompt after owner restart produced %q", got)
				}
			case "join by command":
				env := append(slices.Clone(f.env), "CLAUDE_CODE_SESSION_ID="+key)
				if got := f.command(env, "", "join", "setup-check"); got.exit != 0 {
					t.Fatalf("manual join failed: %s", got.stdout)
				}
			case "end while owner stopped":
				if got := f.invokeHook(repo, "claude", "SessionEnd", `"reason":"other"`); got != "" {
					t.Fatalf("end with owner stopped produced %q", got)
				}
				if pids := f.dispatcherPIDs(); len(pids) != 0 {
					t.Fatalf("end hook restarted the dispatcher: %v", pids)
				}
			}
			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("startup record survived %s: %v", action, err)
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
