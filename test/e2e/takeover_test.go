//go:build e2e

package e2e

import (
	"bytes"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"
)

// An enrolled Codex session takes over a member whose session ended,
// through the real binary, identity layer and installed hooks: the owner
// grants it with one command, the session answers the member's owed ask,
// its next turn runs on the member's route without a new start, and the
// old conversation is told who has the member.
func TestTakeoverThroughTheBinary(t *testing.T) {
	f := newSetupWorld(t)
	f.apply("", "setup", "codex", "--apply")
	repo := f.enrollmentRepo()
	owner := f.owner()
	owner.must("owner", "channel", "create", "dev")
	editor := f.member()
	editor.must("join", "dev", "--as", "editor")

	cli := func(key string, args ...string) outcome {
		t.Helper()
		return f.command(append(slices.Clone(f.env), "CODEX_THREAD_ID="+key), "", args...)
	}
	hook := func(key, event, fields string) {
		t.Helper()
		h := setupHookFor(t, readSetupFile(t, f.paths["codex"]), event)
		cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", h.Command)
		payload := fmt.Sprintf(`{"hook_event_name":%q,"session_id":%q,"cwd":%q,%s}`, event, key, repo, fields)
		cmd.Dir, cmd.Env, cmd.Stdin = repo, f.env, strings.NewReader(payload)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("hook %s %s: %v %s", key, event, err, stderr.String())
		}
	}
	route := func() (string, string) {
		t.Helper()
		var state, reason string
		if err := f.db().QueryRow(`SELECT r.state, r.reason FROM publish_route r JOIN binding b ON b.id = r.binding_id
			WHERE b.alias = 'vega' AND b.state = 'live'`).Scan(&state, &reason); err != nil {
			t.Fatal(err)
		}
		return state, reason
	}

	// The old conversation joined, started, owes a reply, and ended.
	if o := cli("thread-a", "join", "dev", "--as", "vega"); o.exit != 0 {
		t.Fatalf("A join: %+v", o)
	}
	hook("thread-a", "SessionStart", `"source":"startup"`)
	owed := editor.run("ask", "@vega", "Owed work.")
	owedID, _ := owed.obj["id"].(float64)
	hook("thread-a", "SessionEnd", `"reason":"exit"`)
	time.Sleep(1100 * time.Millisecond)

	// The new thread's start hook joins it to the repository's channel.
	for i := 0; ; i++ {
		hook("thread-b", "SessionStart", `"source":"startup"`)
		if o := cli("thread-b", "whoami"); o.exit == 0 {
			break
		}
		if i == 20 {
			t.Fatal("the new thread did not enroll")
		}
		time.Sleep(200 * time.Millisecond)
	}
	hook("thread-b", "UserPromptSubmit", `"turn_id":"b0","prompt":"hi"`)
	hook("thread-b", "Stop", `"turn_id":"b0","stop_hook_active":false`)

	refused := cli("thread-b", "join", "dev", "--move", "--as", "vega")
	if refused.exit == 0 || !strings.Contains(refused.stdout+refused.stderr, "--takeover") {
		t.Fatalf("the taken alias does not offer a takeover: %+v", refused)
	}
	req := cli("thread-b", "join", "dev", "--as", "vega", "--takeover")
	takeover, _ := req.obj["takeover"].(map[string]any)
	if req.exit != 0 || takeover == nil {
		t.Fatalf("takeover request: %+v", req)
	}
	id := fmt.Sprint(int64(takeover["id"].(float64)))
	if again := cli("thread-b", "join", "dev", "--as", "vega", "--takeover"); fmt.Sprint(again.obj["takeover"]) != fmt.Sprint(takeover) {
		t.Fatalf("a repeated request differs: %+v", again)
	}
	if who := owner.mustPlain("owner", "who", "--all"); !strings.Contains(who, "takeover "+id+": #dev/@vega") {
		t.Fatalf("owner who does not list the request:\n%s", who)
	}
	if o := owner.run("owner", "takeover", id); o.exit != 0 {
		t.Fatalf("owner takeover: %+v", o)
	}

	if o := cli("thread-b", "reply", fmt.Sprint(int64(owedID)), "Done by the new session."); o.exit != 0 {
		t.Fatalf("the new session could not answer the member's owed ask: %+v", o)
	}
	hook("thread-b", "UserPromptSubmit", `"turn_id":"b1","prompt":"hi"`)
	hook("thread-b", "Stop", `"turn_id":"b1","stop_hook_active":false`)
	if state, reason := route(); state != "active" {
		t.Fatalf("after the session's next turn without a new start the member's route is %s (%s)", state, reason)
	}
	if o := cli("thread-a", "read"); o.exit == 0 || !strings.Contains(o.stdout+o.stderr, "was given to another session (takeover "+id+")") {
		t.Fatalf("the old conversation was not told: %+v", o)
	}
}
