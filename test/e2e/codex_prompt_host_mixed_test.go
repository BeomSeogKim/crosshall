//go:build e2e

package e2e

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Each names a crosshall built with -tags=testvendorhome from an earlier
// commit. CI builds both: the pull request's base (on main, the previous
// main), and a fixed release from before prompts could name their server,
// whose dispatcher refuses the fields a hook must not send it unasked.
const (
	baseBinaryEnv    = "CROSSHALL_E2E_BASE_BINARY"
	releaseBinaryEnv = "CROSSHALL_E2E_RELEASE_BINARY"
)

// An upgrade in place while a session runs: crosshall of the base commit is
// installed and runs the dispatcher, then the binary the hooks name is
// replaced by this build. Its hooks, under the dispatcher that keeps
// running, must stay fail-open: its prompts reach the hall, print nothing
// about the other build into the session, and the session still reads its
// messages. An older dispatcher decodes hook.event strictly, so a hook that
// sends a field the dispatcher does not offer to accept loses the prompt
// (as a hook sending the prompt's server unasked would, #252).
func TestNewCodexHookUnderAnOlderDispatcherStaysFailOpen(t *testing.T) {
	for _, env := range []string{baseBinaryEnv, releaseBinaryEnv} {
		t.Run(env, func(t *testing.T) {
			old := os.Getenv(env)
			if old == "" {
				t.Skip("set " + env + " to run a new hook against that older dispatcher")
			}
			upgradeInPlaceUnderOlderDispatcher(t, old)
		})
	}
}

func upgradeInPlaceUnderOlderDispatcher(t *testing.T, old string) {
	f := installSetupWorld(newWorld(t), old)
	f.apply("", "setup", "codex", "--apply")
	repo := f.enrollmentRepo()
	f.codexServer = buildCodexDaemon(t, filepath.Dir(f.paths["codex"]))
	dispatcher := f.dispatcherPIDs()
	if len(dispatcher) != 1 {
		t.Fatalf("want the base build's dispatcher running, got %v", dispatcher)
	}
	// Replaced by rename, as an installer does: the running dispatcher
	// keeps the base build.
	content, err := os.ReadFile(f.binaryPath())
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
	// A note sent across the upgrade must reach the session.
	const body = "fixture note across the upgrade"
	writer := f.member()
	writer.must("join", "setup-check", "--as", "writer")
	writer.must("note", "@codex1", body)
	// A turn end may name its server too (#437); the older dispatcher does
	// not offer to accept that, so the hook must not send it.
	for _, turn := range []string{"turn-1", "turn-2"} {
		for _, call := range []struct{ event, fields, kind string }{
			{"UserPromptSubmit", `"turn_id":"` + turn + `","prompt":"fixture private text"`, "prompt_submitted"},
			{"Stop", `"turn_id":"` + turn + `","stop_hook_active":false`, "turn_finishing"},
		} {
			out := f.invokeHook(repo, "codex", call.event, call.fields)
			for _, word := range []string{"refus", "invalid", "error", "bad arguments"} {
				if strings.Contains(strings.ToLower(out), word) {
					t.Fatalf("the %s printed %q into the session", call.event, out)
				}
			}
			waitObservation(t, f, db, turn, call.kind)
		}
	}
	member := &participant{w: f.world, extra: []string{"CODEX_THREAD_ID=" + setupSessionPrefix + "codex"}}
	if got := member.run("read"); got.exit != 0 || !strings.Contains(got.stdout, body) {
		t.Fatalf("the session did not read the note: exit %d\n%s%s", got.exit, got.stdout, got.stderr)
	}
	if got := f.dispatcherPIDs(); len(got) != 1 || got[0] != dispatcher[0] {
		t.Fatalf("dispatchers %v, want the base build's %d throughout", got, dispatcher[0])
	}
}

func waitObservation(t *testing.T, f *setupWorld, db *sql.DB, turn, kind string) {
	t.Helper()
	for deadline := time.Now().Add(fixtureEventTimeout); ; time.Sleep(50 * time.Millisecond) {
		var observed int
		if err := db.QueryRow(`SELECT COUNT(*) FROM observation WHERE turn_ref = ? AND kind = ?`, turn, kind).
			Scan(&observed); err == nil && observed > 0 {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("the dispatcher did not record %s of %s; %s", kind, turn, f.enrollmentEvidence("codex"))
		}
	}
}

// The other direction: the hooks still name the base commit's crosshall
// while this build's dispatcher runs. A hook from before #437 names no
// server on a turn end, so a hand-joined session gets nothing from it and
// its next prompt registers the route; a later one registers it at the turn
// end, and the prompt keeps it. Either way the session has one route.
func TestOlderCodexHookUnderANewDispatcherRegistersAtItsPrompt(t *testing.T) {
	old := os.Getenv(baseBinaryEnv)
	if old == "" {
		t.Skip("set " + baseBinaryEnv + " to run that older hook under this dispatcher")
	}
	f := newSetupWorld(t)
	f.apply("", "setup", "codex", "--apply")
	repo := f.enrollmentRepo()
	f.codexServer = buildCodexDaemon(t, filepath.Dir(f.paths["codex"]))
	dispatcher := f.dispatcherPIDs()
	if len(dispatcher) != 1 {
		t.Fatalf("want this build's dispatcher running, got %v", dispatcher)
	}
	content, err := os.ReadFile(old)
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
	member := &participant{w: f.world, extra: []string{"CODEX_THREAD_ID=" + setupSessionPrefix + "codex"}}
	member.must("join", "setup-check", "--as", "codex1")
	db := f.db()
	routes := func() int {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM publish_route WHERE adapter = 'codex' AND host_pid IS NOT NULL`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	f.invokeHook(repo, "codex", "Stop", `"turn_id":"turn-1","stop_hook_active":false`)
	waitObservation(t, f, db, "turn-1", "turn_finishing")
	if n := routes(); n > 1 {
		t.Fatalf("the older hook's turn end registered %d routes", n)
	}
	f.invokeHook(repo, "codex", "UserPromptSubmit", `"turn_id":"turn-2","prompt":"fixture private text"`)
	waitObservation(t, f, db, "turn-2", "prompt_submitted")
	if n := routes(); n != 1 {
		t.Fatalf("the older hook left %d routes after its prompt, want the server's", n)
	}
	if got := f.dispatcherPIDs(); len(got) != 1 || got[0] != dispatcher[0] {
		t.Fatalf("dispatchers %v, want this build's %d throughout", got, dispatcher[0])
	}
}
