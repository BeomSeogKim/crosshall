//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// baseBinaryEnv names a crosshall built with -tags=testvendorhome from a
// commit before the prompt's server was offered.
const baseBinaryEnv = "CROSSHALL_E2E_BASE_BINARY"

// A new Codex hook under an older dispatcher, which decodes hook.event
// strictly and does not offer the prompt's server (#252): the hook sends no
// server, so its prompts still reach the hall, print nothing about it into
// the session, and the session still reads its messages.
func TestNewCodexHookUnderAnOlderDispatcherStaysFailOpen(t *testing.T) {
	old := os.Getenv(baseBinaryEnv)
	if old == "" {
		t.Skip("set " + baseBinaryEnv + " to run a new hook against an older dispatcher")
	}
	f := newSetupWorld(t)
	f.apply("", "setup", "codex", "--apply")
	repo := f.enrollmentRepo()
	f.codexServer = buildCodexDaemon(t, filepath.Dir(f.paths["codex"]))

	f.kill()
	start := exec.CommandContext(t.Context(), old, "owner", "start")
	start.Env, start.Dir = f.env, f.home
	if out, err := start.CombinedOutput(); err != nil {
		t.Fatalf("start the older dispatcher: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(filepath.Join(f.rendezvous(), "ready"))
	if err != nil {
		t.Fatal(err)
	}
	var ready struct {
		Hello []string `json:"hello"`
	}
	if err := json.Unmarshal(raw, &ready); err != nil || slices.Contains(ready.Hello, "prompt_host") {
		t.Fatalf("the dispatcher is not an older one: %s (%v)", raw, err)
	}

	db := f.db()
	for deadline := time.Now().Add(fixtureEventTimeout); ; time.Sleep(100 * time.Millisecond) {
		f.invokeHook(repo, "codex", "SessionStart", `"source":"startup"`)
		var routes int
		if err := db.QueryRow(`SELECT COUNT(*) FROM publish_route WHERE adapter = 'codex' AND state = 'active'`).Scan(&routes); err == nil && routes == 1 {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("the start did not join under the older dispatcher; %s", f.enrollmentEvidence("codex"))
		}
	}
	for _, turn := range []string{"turn-1", "turn-2"} {
		out := f.invokeHook(repo, "codex", "UserPromptSubmit", `"turn_id":"`+turn+`","prompt":"fixture private text"`)
		for _, word := range []string{"refus", "invalid", "error", "bad arguments"} {
			if strings.Contains(strings.ToLower(out), word) {
				t.Fatalf("the prompt printed %q into the session", out)
			}
		}
		for deadline := time.Now().Add(fixtureEventTimeout); ; time.Sleep(50 * time.Millisecond) {
			var observed int
			if err := db.QueryRow(`SELECT COUNT(*) FROM observation WHERE turn_ref = ? AND kind = 'prompt_submitted'`, turn).
				Scan(&observed); err == nil && observed > 0 {
				break
			}
			if !time.Now().Before(deadline) {
				t.Fatalf("the older dispatcher did not record prompt %s; %s", turn, f.enrollmentEvidence("codex"))
			}
		}
	}
	member := &participant{w: f.world, extra: []string{"CODEX_THREAD_ID=" + setupSessionPrefix + "codex"}}
	if got := member.run("read"); got.exit != 0 {
		t.Fatalf("the session cannot read its messages: exit %d\n%s%s", got.exit, got.stdout, got.stderr)
	}
}
