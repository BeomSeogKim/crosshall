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
			// A second callback with the recovered prompt's id repeats nothing.
			// Claude's hook tells it apart by its entry time (#163: a prompt
			// typed while that turn runs), so it is observed; Codex's is a replay.
			if got := f.invokeHook(repo, test.adapter, "UserPromptSubmit", prompt(recovered)); got != "" {
				t.Fatalf("a second callback of the turn returned %q", got)
			}
			if got := f.invokeHook(repo, test.adapter, "UserPromptSubmit", prompt("ordinary")); got != "" {
				t.Fatalf("established session received a second welcome %q", got)
			}

			db := f.db()
			var bindings, launches, observations, turns, routes int
			if err := db.QueryRow(`SELECT
				(SELECT COUNT(*) FROM binding WHERE principal_id = k.principal_id AND state = 'live'),
				(SELECT COUNT(*) FROM adapter_launch WHERE principal_id = k.principal_id AND launch_ref = 'session' AND verified = 0
					AND started_hook_at_ms = 0 AND json_extract(profile, '$.mode') = 'unverified'),
				(SELECT COUNT(*) FROM observation WHERE principal_id = k.principal_id AND turn_ref = ?1),
				(SELECT COUNT(*) FROM turn WHERE principal_id = k.principal_id AND turn_ref = ?1),
				(SELECT COUNT(*) FROM publish_route WHERE principal_id = k.principal_id)
				FROM principal_key k WHERE vendor = ?2 AND key = ?3`, recovered, test.adapter, key).Scan(&bindings, &launches, &observations, &turns, &routes); err != nil ||
				bindings != 1 || launches != 1 || turns != 1 || routes != 0 {
				t.Fatalf("recovery wrote bindings %d, launches %d, turns %d, routes %d (%v)", bindings, launches, turns, routes, err)
			}
			// Claude's second callback is its own event unless both hooks
			// entered in the same millisecond, which gives them one reference.
			if want := map[string][2]int{"claude": {1, 2}, "codex": {1, 1}}[test.adapter]; observations < want[0] || observations > want[1] {
				t.Fatalf("recovery observed the prompt %d times, want %d to %d", observations, want[0], want[1])
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
	path := f.startupRepositoryFile(adapter)
	want := "-\n"
	if repo != "" {
		want = fmt.Sprintf("%x\n", sha256.Sum256([]byte(repo)))
	}
	deadline := time.Now().Add(fixtureEventTimeout)
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

func (f *setupWorld) startupRepositoryFile(adapter string) string {
	id := ident.Identity{Vendor: adapter, Key: setupSessionPrefix + adapter}
	return filepath.Join(f.state, "hook-start-repos", id.Digest())
}

func (f *setupWorld) waitForStartupRecordRemoval(cwd string) {
	f.t.Helper()
	path := f.startupRepositoryFile("claude")
	for deadline := time.Now().Add(fixtureEventTimeout); ; {
		if got := f.invokeHook(cwd, "claude", "SessionEnd", `"reason":"other"`); got != "" {
			f.t.Fatalf("SessionEnd produced %q", got)
		}
		_, err := os.Lstat(path)
		if os.IsNotExist(err) {
			return
		}
		if err != nil || !time.Now().Before(deadline) {
			f.t.Fatalf("SessionEnd left the startup record (%v); counter %q", err, f.hookCounter("claude", "SessionEnd"))
		}
		f.waitEnrollmentPackets("claude", "SessionEnd", deadline)
	}
}

func (f *setupWorld) waitForEnrollmentWelcome(cwd, adapter, event string, fields func(string) string) string {
	f.t.Helper()
	deadline := time.Now().Add(fixtureEventTimeout)
	delivered := f.waitForWelcomeDelivery(adapter, event, deadline, func(turn string) string {
		return f.invokeHook(cwd, adapter, event, fields(turn))
	})
	db := f.db()
	f.waitEnrollmentPackets(adapter, event, deadline)
	if _, welcome := hookJoinAcknowledgements(f.t, db, adapter); !welcome {
		// A member request can confirm the native receipt if its one-way
		// acknowledgement expired. A wait carries no member rules, so the
		// session's first command still gets them.
		identity := "CLAUDE_CODE_SESSION_ID"
		if adapter == "codex" {
			identity = "CODEX_THREAD_ID"
		}
		member := &participant{w: f.world, extra: []string{identity + "=" + setupSessionPrefix + adapter}}
		member.run("wait", "--existing", "--timeout", "1s")
		if _, welcome := hookJoinAcknowledgements(f.t, db, adapter); !welcome {
			f.t.Fatalf("%s welcome from %q was not acknowledged; counter %q, %s", event, delivered, f.hookCounter(adapter, event), f.enrollmentEvidence(adapter))
		}
	}
	return delivered
}

func (f *setupWorld) waitForWelcomeDelivery(adapter, event string, deadline time.Time, invoke func(string) string) string {
	f.t.Helper()
	db := f.db()
	for attempt := 1; time.Now().Before(deadline); attempt++ {
		turn := fmt.Sprintf("recovery-%d", attempt)
		out := invoke(turn)
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
			if !strings.Contains(native.Output.Context, "Joined #setup-check as @"+adapter+"1") {
				f.t.Fatalf("%s did not deliver the welcome: %q; %s", event, out, f.enrollmentEvidence(adapter))
			}
			if strings.Contains(native.Output.Context, "Before voluntarily ending any turn") {
				f.t.Fatalf("%s start line carried the member rules: %q", event, out)
			}
			// The callback can expire between stdout and the local receipt.
			if hasWelcomeReceipt(f.t, f.enrollmentLeaf(adapter)) {
				return turn
			}
			if _, acknowledged := hookJoinAcknowledgements(f.t, db, adapter); acknowledged {
				return turn
			}
		}
		f.waitEnrollmentPackets(adapter, event, deadline)
	}
	f.t.Fatalf("no receipted %s recovery welcome (counter %q); %s", event, f.hookCounter(adapter, event), f.enrollmentEvidence(adapter))
	return ""
}

func hasWelcomeReceipt(t *testing.T, leaf string) bool {
	t.Helper()
	info, err := os.Lstat(filepath.Join(leaf, "seen-join-hint"))
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().IsRegular()
}

// A directory at the receipt's path prevents its atomic rename after stdout.
func blockWelcomeReceipt(t *testing.T, leaf string) func() {
	t.Helper()
	path := filepath.Join(leaf, "seen-join-hint")
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	return func() {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
}

func TestInstalledHookRetriesWelcomePrintedWithoutReceipt(t *testing.T) {
	f := newSetupWorld(t)
	f.apply("", "setup", "claude", "--apply")
	repo := f.enrollmentRepo()
	member := &participant{w: f.world, extra: []string{"CLAUDE_CODE_SESSION_ID=" + setupSessionPrefix + "claude"}}
	if res := member.run("join", "missing-channel"); res.exit == 0 {
		t.Fatal("provisional join unexpectedly succeeded")
	}
	leaf := f.enrollmentLeaf("claude")
	unblock := blockWelcomeReceipt(t, leaf)
	db := f.db()
	var missed, acknowledged string
	deadline := time.Now().Add(fixtureEventTimeout)
	confirmed := f.waitForWelcomeDelivery("claude", "SessionStart", deadline, func(turn string) string {
		if acknowledged != "" {
			t.Fatalf("retried with %q after the store confirmed %q", turn, acknowledged)
		}
		out := f.invokeHook(repo, "claude", "SessionStart", `"source":"startup"`)
		if out == "" {
			return out
		}
		if missed == "" {
			missed = turn
			unblock()
		} else if hasWelcomeReceipt(t, leaf) {
			f.waitEnrollmentPackets("claude", "SessionStart", deadline)
			if _, seen := hookJoinAcknowledgements(t, db, "claude"); !seen {
				member.run("wait", "--existing", "--timeout", "1s")
				if _, seen := hookJoinAcknowledgements(t, db, "claude"); !seen {
					t.Fatal("the member did not confirm the native receipt")
				}
			}
			// Only the store can confirm this delivery after the file is gone.
			if err := os.Remove(filepath.Join(leaf, "seen-join-hint")); err != nil {
				t.Fatal(err)
			}
			acknowledged = turn
		}
		return out
	})
	if missed == "" || confirmed == missed || confirmed != acknowledged {
		t.Fatalf("welcome without receipt on %q, stored on %q, accepted on %q", missed, acknowledged, confirmed)
	}
}

func (f *setupWorld) waitEnrollmentPackets(adapter, event string, deadline time.Time) {
	f.t.Helper()
	leaf := f.enrollmentLeaf(adapter)
	for {
		packets := fixtureQueuedPackets(f.t, leaf)
		if len(packets) == 0 {
			return
		}
		if !time.Now().Before(deadline) {
			f.t.Fatalf("%s %s packets still queued: %v; last counter %q", adapter, event, packets, f.hookCounter(adapter, event))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (f *setupWorld) enrollmentLeaf(adapter string) string {
	id := ident.Identity{Vendor: adapter, Key: setupSessionPrefix + adapter}
	return filepath.Join(f.rendezvous(), "m", id.Digest()[:16])
}

func recoveryPrompt(turn string) string {
	return fmt.Sprintf(`"prompt_id":%q`, turn)
}

func (f *setupWorld) waitForUnmatchedStart(cwd, fields string) {
	f.t.Helper()
	counterBytes := func() int64 {
		entries, err := os.ReadDir(filepath.Join(f.rendezvous(), "hooks"))
		if err != nil && !os.IsNotExist(err) {
			f.t.Fatal(err)
		}
		var size int64
		for _, entry := range entries {
			info, err := entry.Info()
			if err != nil {
				f.t.Fatal(err)
			}
			if info.Mode().IsRegular() {
				size += info.Size()
			}
		}
		return size
	}
	for deadline := time.Now().Add(fixtureEventTimeout); ; {
		before := counterBytes()
		out := f.invokeHook(cwd, "claude", "SessionStart", fields)
		f.waitEnrollmentPackets("claude", "SessionStart", deadline)
		stage := f.enrollmentEvidence("claude")
		if out != "" || !strings.Contains(stage, "no registered leaf") {
			f.t.Fatalf("unmatched start %s produced %q, %s", fields, out, stage)
		}
		if counterBytes() > before && strings.HasPrefix(f.hookCounter("claude", "SessionStart"), "claude SessionStart unmatched ") {
			return
		}
		if !time.Now().Before(deadline) {
			f.t.Fatalf("start %s never reached the unmatched decision; %s", fields, stage)
		}
	}
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
				// Setup's policy updates share one 3 s budget with a dispatcher
				// start; under load a cold start used it up. This test is about
				// later starts, so the dispatcher runs before setup.
				f.owner().must("owner", "start")
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
					f.waitForStartupRecordRemoval(start)
					wantJoin = true
				}
				fields := fmt.Sprintf(`"source":%q`, source)
				if wantJoin {
					f.waitForEnrollmentWelcome(later, "claude", "SessionStart", func(string) string { return fields })
					return
				}
				f.waitForUnmatchedStart(later, fields)
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
			marker := f.startupRepositoryFile("claude")
			owner := f.owner()
			if action == "join after owner restart" || action == "end while owner stopped" {
				owner.must("owner", "stop")
			}
			start, startupRepo := repo, repo
			if action == "join by command" {
				start, startupRepo = f.home, ""
			}
			if action == "join on start" {
				f.waitForEnrollmentWelcome(start, "claude", "SessionStart", func(string) string { return `"source":"startup"` })
			} else {
				f.waitForStartupRepository(start, "claude", "startup", startupRepo)
			}
			if action == "join after owner restart" || action == "end while owner stopped" {
				if pids := f.dispatcherPIDs(); len(pids) != 0 {
					t.Fatalf("start hook restarted the dispatcher: %v", pids)
				}
			}
			switch action {
			case "join after owner restart":
				owner.must("owner", "start")
				f.waitForEnrollmentWelcome(repo, "claude", "UserPromptSubmit", recoveryPrompt)
			case "join by command":
				env := append(slices.Clone(f.env), "CLAUDE_CODE_SESSION_ID="+key)
				if got := f.command(env, "", "join", "setup-check"); got.exit != 0 {
					t.Fatalf("manual join failed: %s", got.stdout)
				}
			case "end while owner stopped":
				f.waitForStartupRecordRemoval(repo)
				if pids := f.dispatcherPIDs(); len(pids) != 0 {
					t.Fatalf("end hook restarted the dispatcher: %v", pids)
				}
			}
			if _, err := os.Lstat(marker); !os.IsNotExist(err) {
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
