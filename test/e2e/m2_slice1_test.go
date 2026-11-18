//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/ident"
)

// The issue #13 checks end to end: the real hook binary, the real dispatcher
// and the real domain, a synthetic Claude Code session in a temporary home.

type claudeSession struct {
	w        *world
	key, pid string
	repo     string
}

// fire runs `crosshall hook claude <event>` as an installed hook would and
// returns what it printed for the host.
func (c claudeSession) fire(event string, fields map[string]any) string {
	c.w.t.Helper()
	payload := map[string]any{"hook_event_name": event, "session_id": c.key, "cwd": c.repo}
	for k, v := range fields {
		payload[k] = v
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		c.w.t.Fatal(err)
	}
	cmd := exec.CommandContext(c.w.t.Context(), binary, "hook", "claude", event)
	var stdout, stderr bytes.Buffer
	cmd.Stdin, cmd.Stdout, cmd.Stderr = bytes.NewReader(raw), &stdout, &stderr
	cmd.Dir = c.repo
	cmd.Env = append(c.w.baseEnv(), "CLAUDE_CODE_SESSION_ID="+c.key, "CLAUDE_PID="+c.pid)
	if err := cmd.Run(); err != nil || stderr.Len() != 0 {
		c.w.t.Fatalf("hook %s: %v, stderr %q", event, err, stderr.String())
	}
	return stdout.String()
}

// settle waits until the dispatcher has claimed every one-way observation the
// session's hooks published, so table counts are stable.
func (c claudeSession) settle() {
	c.w.t.Helper()
	digest := (ident.Identity{Vendor: "claude", Key: c.key}).Digest()
	in := filepath.Join(c.w.rendezvous(), "m", digest[:16], "in")
	work := filepath.Join(c.w.rendezvous(), "m", digest[:16], "work")
	deadline := time.Now().Add(3 * time.Second)
	for {
		a, _ := os.ReadDir(in)
		b, _ := os.ReadDir(work)
		if len(a) == 0 && len(b) == 0 {
			return
		}
		if time.Now().After(deadline) {
			c.w.t.Fatalf("one-way observations still queued: %d in, %d work", len(a), len(b))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// lastCounter is "<event> <outcome>" of the most recent hook counter record.
func (c claudeSession) lastCounter() string {
	c.w.t.Helper()
	dir := filepath.Join(c.w.rendezvous(), "hooks")
	logs, err := os.ReadDir(dir)
	if err != nil || len(logs) == 0 {
		c.w.t.Fatalf("no hook counters: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, logs[len(logs)-1].Name()))
	if err != nil {
		c.w.t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	f := strings.Fields(lines[len(lines)-1])
	if len(f) != 4 {
		c.w.t.Fatalf("counter record %q", lines[len(lines)-1])
	}
	return f[1] + " " + f[2]
}

// hostContext is the additionalContext a hook printed, or "" for silence.
func hostContext(t *testing.T, out string) string {
	t.Helper()
	if out == "" {
		return ""
	}
	var v struct {
		Output struct {
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil || v.Output.Context == "" {
		t.Fatalf("hook output is not one context object: %q (%v)", out, err)
	}
	return v.Output.Context
}

func m2World(t *testing.T) (*world, *participant, claudeSession) {
	w := newWorld(t)
	owner := w.owner()
	owner.must("owner", "start")
	owner.must("owner", "channel", "create", "checkout")
	repo := filepath.Join(w.home, "checkout")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	owner.must("owner", "channel", "default", "checkout", "--repo", repo)
	owner.must("owner", "adapter", "set", "claude", "--enabled", "on")
	writer := w.member()
	writer.must("join", "checkout", "--as", "writer")
	return w, writer, claudeSession{w: w, key: "synthetic-m2-session", pid: "4242", repo: repo}
}

func notes(t *testing.T, from *participant, alias string, n int) []string {
	var ids []string
	for i := 0; i < n; i++ {
		ids = append(ids, strconv.FormatInt(num(t, from.must("note", "@"+alias, "synthetic note "+strconv.Itoa(i)), "id"), 10))
	}
	return ids
}

func pendingContext(ids []string) string { return "[crosshall] pending: " + strings.Join(ids, " ") }

func rosterHas(t *testing.T, viewer *participant, alias string) bool {
	for _, a := range viewer.must("who").aliases() {
		if a == alias {
			return true
		}
	}
	return false
}

func tableCounts(t *testing.T, w *world) map[string]int {
	t.Helper()
	db := w.db()
	out := map[string]int{}
	for _, table := range []string{"observation", "offer", "offer_item", "roster_change", "adapter_launch", "turn", "binding", "event"} {
		var n int
		if err := db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[table] = n
	}
	return out
}

func TestM2SessionThroughTheHookBinary(t *testing.T) {
	w, writer, c := m2World(t)

	if got := hostContext(t, c.fire("SessionStart", map[string]any{"source": "startup"})); !strings.Contains(got, "Joined #checkout as @claude1") {
		t.Fatalf("auto-join context %q", got)
	}
	mail := notes(t, writer, "claude1", 2)
	if got := hostContext(t, c.fire("UserPromptSubmit", map[string]any{"prompt_id": "p1", "prompt": "synthetic"})); got != pendingContext(mail) {
		t.Fatalf("first prompt %q", got)
	}
	// Mail during a turn is offered at the next prompt even though a stop came first.
	late := notes(t, writer, "claude1", 1)
	if got := c.fire("Stop", map[string]any{"prompt_id": "p1", "stop_hook_active": false}); got != "" {
		t.Fatalf("stop printed %q", got)
	}
	if got := hostContext(t, c.fire("UserPromptSubmit", map[string]any{"prompt_id": "p2"})); got != pendingContext(late) {
		t.Fatalf("prompt after stop %q", got)
	}
	// Eleven unread across two prompts, none lost.
	eleven := notes(t, writer, "claude1", 11)
	if got := hostContext(t, c.fire("UserPromptSubmit", map[string]any{"prompt_id": "p3"})); got != pendingContext(eleven[:10]) {
		t.Fatalf("ten oldest %q", got)
	}
	if got := hostContext(t, c.fire("UserPromptSubmit", map[string]any{"prompt_id": "p4"})); got != pendingContext(eleven[10:]) {
		t.Fatalf("the eleventh %q", got)
	}
	// A quiet turn: nothing printed.
	if got := c.fire("UserPromptSubmit", map[string]any{"prompt_id": "p5"}); got != "" {
		t.Fatalf("quiet prompt printed %q", got)
	}
	// ... and made no dispatcher round trip: the stamp matched the last notice ack.
	if last := c.lastCounter(); last != "UserPromptSubmit clean" {
		t.Fatalf("quiet prompt counted as %q, want a clean stamp check", last)
	}
	// The turn's stop, with the reminder off, publishes without waiting either.
	if got := c.fire("Stop", map[string]any{"prompt_id": "p5", "stop_hook_active": false}); got != "" {
		t.Fatalf("quiet stop printed %q", got)
	}
	if last := c.lastCounter(); last != "Stop clean" {
		t.Fatalf("quiet stop counted as %q, want no round trip", last)
	}
	// A second replay of every callback that carries a native reference
	// prints nothing and changes nothing. (Starts and ends carry none: the
	// hook's own time tells them apart, so a replay of one is a new event.)
	c.settle()
	before := tableCounts(t, w)
	for _, p := range []string{"p1", "p2", "p3", "p4", "p5"} {
		if got := c.fire("UserPromptSubmit", map[string]any{"prompt_id": p}); got != "" {
			t.Fatalf("replayed prompt %s printed %q", p, got)
		}
	}
	if got := c.fire("Stop", map[string]any{"prompt_id": "p1", "stop_hook_active": false}); got != "" {
		t.Fatalf("replayed stop printed %q", got)
	}
	c.settle()
	if after := tableCounts(t, w); !reflect.DeepEqual(before, after) {
		t.Fatalf("replay changed rows: %v -> %v", before, after)
	}

	// A clean end of the bound launch departs; the next start restores.
	c.fire("SessionEnd", map[string]any{"reason": "other"})
	if rosterHas(t, writer, "claude1") {
		t.Fatal("a clean end did not depart the member")
	}
	c.fire("SessionStart", map[string]any{"source": "resume"})
	if !rosterHas(t, writer, "claude1") {
		t.Fatal("the next start did not restore the member")
	}
}

func TestM2HookFailuresPrintNothingAndAreCounted(t *testing.T) {
	w, _, c := m2World(t)
	c.fire("SessionStart", map[string]any{"source": "startup"})

	// Conflicting identity: the environment names another session.
	other := c
	other.key = "synthetic-other"
	cmd := exec.CommandContext(t.Context(), binary, "hook", "claude", "UserPromptSubmit")
	cmd.Stdin = strings.NewReader(`{"hook_event_name":"UserPromptSubmit","session_id":"synthetic-m2-session","prompt_id":"x"}`)
	cmd.Dir, cmd.Env = c.repo, append(w.baseEnv(), "CLAUDE_CODE_SESSION_ID=synthetic-other", "CLAUDE_PID=4242")
	if out, err := cmd.Output(); err != nil || len(out) != 0 {
		t.Fatalf("conflicting identity: %q, %v", out, err)
	}
	// Malformed input.
	cmd = exec.CommandContext(t.Context(), binary, "hook", "claude", "UserPromptSubmit")
	cmd.Stdin = strings.NewReader(`{"broken"`)
	cmd.Dir, cmd.Env = c.repo, append(w.baseEnv(), "CLAUDE_CODE_SESSION_ID="+c.key, "CLAUDE_PID=4242")
	if out, err := cmd.Output(); err != nil || len(out) != 0 {
		t.Fatalf("malformed input: %q, %v", out, err)
	}
	// Switched off.
	w.owner().must("owner", "adapter", "set", "claude", "--enabled", "off")
	if got := c.fire("UserPromptSubmit", map[string]any{"prompt_id": "off-1"}); got != "" {
		t.Fatalf("switched-off adapter printed %q", got)
	}
	// No dispatcher: the adapter is on, the dispatcher is gone.
	w.owner().must("owner", "adapter", "set", "claude", "--enabled", "on")
	w.kill()
	if got := c.fire("UserPromptSubmit", map[string]any{"prompt_id": "dead-1"}); got != "" {
		t.Fatalf("no dispatcher printed %q", got)
	}
	logs, err := os.ReadDir(filepath.Join(w.rendezvous(), "hooks"))
	if err != nil || len(logs) == 0 {
		t.Fatalf("no hook counters: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(w.rendezvous(), "hooks", logs[0].Name()))
	if err != nil {
		t.Fatal(err)
	}
	for _, outcome := range []string{" identity ", " refused ", " off "} {
		if !strings.Contains(string(raw), outcome) {
			t.Errorf("counter outcome %q missing from\n%s", strings.TrimSpace(outcome), raw)
		}
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	last := strings.Fields(lines[len(lines)-1])
	if len(last) != 4 || last[2] != "no_dispatcher" {
		t.Errorf("the call without a dispatcher was counted as %v", last)
	} else if ms, _ := strconv.Atoi(last[3]); ms > 50 {
		t.Errorf("the call without a dispatcher waited %d ms", ms)
	}
	if strings.Contains(string(raw), c.key) || strings.Contains(string(raw), "synthetic note") {
		t.Fatal("a counter record carries an identity or content")
	}
}
