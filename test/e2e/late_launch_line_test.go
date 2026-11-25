//go:build e2e

package e2e

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// A request that arrives while a member's session has no open launch marks
// nothing for its hook. The first prompt of the session's next process binds
// that launch late and must still show the request, not answer from its old
// stamp.
func TestALatePromptShowsWorkThatCameWithoutALaunch(t *testing.T) {
	w, writer, c := m2World(t)
	if got := c.startWithContext("startup"); got == "" {
		t.Fatal("the start printed no context")
	}
	if got := hostContext(t, c.fire("UserPromptSubmit", map[string]any{"prompt_id": "first"})); got != "" {
		t.Fatalf("a prompt with nothing pending printed %q", got)
	}
	c.waitForConsumption(time.Now().Add(fixtureEventTimeout))

	// The process's launch ends while the member stays: its next start hook
	// will be missed, as under load.
	writable, err := sql.Open("sqlite", "file:"+filepath.Join(w.state, "store.sqlite3")+"?mode=rw&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatal(err)
	}
	_, err = writable.Exec(`UPDATE adapter_launch SET ended_hook_at_ms = ? WHERE ended_hook_at_ms IS NULL AND principal_id =
		(SELECT principal_id FROM principal_key WHERE vendor = 'claude' AND key = ?)`, time.Now().UnixMilli(), c.key)
	if err := errors.Join(err, writable.Close()); err != nil {
		t.Fatal(err)
	}
	ids := notes(t, writer, "claude1", 1)

	// The resumed process is a shell that runs the hook as its child, so
	// the hook is hosted by a process with no launch record yet.
	payload, err := json.Marshal(map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": c.key, "cwd": c.repo, "prompt_id": "resumed"})
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", `CLAUDE_PID=$$ "$0" hook claude UserPromptSubmit; status=$?; exit $status`, binary)
	var stdout, stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr, cmd.Dir = bytes.NewReader(payload), &stdout, &stderr, c.repo
	cmd.Env = append(w.baseEnv(), "CLAUDE_CODE_SESSION_ID="+c.key)
	if err := cmd.Run(); err != nil || stderr.Len() != 0 {
		t.Fatalf("resumed prompt hook: %v, stderr %q", err, stderr.String())
	}
	if got := hostContext(t, stdout.String()); got != pendingContext(ids) {
		t.Fatalf("the first prompt of the resumed process printed %q, want %q (counter %q)", got, pendingContext(ids), c.lastCounter())
	}
}
