//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// baseBinaryEnv names a crosshall built with -tags=testvendorhome from an
// earlier commit; CI builds the pull request's base (or, on main, the
// previous main).
const baseBinaryEnv = "CROSSHALL_E2E_BASE_BINARY"

// An upgrade in place while a session runs: crosshall of the base commit is
// installed and runs the dispatcher, then the binary the hooks name is
// replaced by this build. Its hooks, under the dispatcher that keeps
// running, must stay fail-open: its prompts reach the hall, print nothing
// about the other build into the session, and the session still reads its
// messages. An older dispatcher decodes hook.event strictly, so a hook that
// sends a field the dispatcher does not offer to accept loses the prompt
// (as a hook sending the prompt's server unasked would, #252).
func TestNewCodexHookUnderAnOlderDispatcherStaysFailOpen(t *testing.T) {
	old := os.Getenv(baseBinaryEnv)
	if old == "" {
		t.Skip("set " + baseBinaryEnv + " to run a new hook against an older dispatcher")
	}
	head := binary
	binary = old // newSetupWorld installs it; its setup starts its dispatcher
	t.Cleanup(func() { binary = head })
	f := newSetupWorld(t)
	binary = head
	f.apply("", "setup", "codex", "--apply")
	repo := f.enrollmentRepo()
	f.codexServer = buildCodexDaemon(t, filepath.Dir(f.paths["codex"]))
	dispatcher := f.dispatcherPIDs()
	if len(dispatcher) != 1 {
		t.Fatalf("want the base build's dispatcher running, got %v", dispatcher)
	}
	// Replaced by rename, as an installer does: the running dispatcher
	// keeps the base build.
	content, err := os.ReadFile(head)
	if err != nil {
		t.Fatal(err)
	}
	next := f.executable + ".next"
	if err := os.WriteFile(next, content, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, f.executable); err != nil {
		t.Fatal(err)
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
	if got := f.dispatcherPIDs(); len(got) != 1 || got[0] != dispatcher[0] {
		t.Fatalf("dispatchers %v, want the base build's %d throughout", got, dispatcher[0])
	}
}
