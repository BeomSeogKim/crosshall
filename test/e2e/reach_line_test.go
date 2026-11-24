//go:build e2e

package e2e

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/domain"
)

// #121: a session the hall can start a turn in is told so at a prompt, the
// line is stored once the real hook has printed it, and the next prompt
// after the owner switches notices off says the hall cannot reach it.
func TestReachLineFollowsWhetherTheHallReachesTheSession(t *testing.T) {
	const version, manifest, mode = "2.1.9270", "claude-3", "interactive"
	tmp, err := os.MkdirTemp("/tmp", "ch-reach-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(tmp) })
	if err := os.Mkdir(filepath.Join(tmp, "cc-socks"), 0700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(tmp, "cc-socks", "fixture.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	w := newWorld(t)
	owner := w.owner()
	owner.extra = []string{"TMPDIR=" + tmp}
	owner.must("owner", "start")
	owner.must("owner", "channel", "create", "checkout")
	repo := filepath.Join(w.home, "checkout")
	if out, err := exec.CommandContext(t.Context(), "git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	owner.must("owner", "channel", "default", "checkout", "--repo", repo)
	owner.must("owner", "adapter", "set", "claude", "--enabled", "on")
	evidence, err := json.Marshal(domain.AdapterEvidence{Vendor: "claude", VendorVersion: version,
		Platform: runtime.GOOS + "-" + runtime.GOARCH, Mode: mode, ManifestRev: manifest, RunAtMS: time.Now().UnixMilli(),
		Results: []domain.EvidenceResult{{Cap: "publish", Level: "tested", Check: "synthetic fixture evidence"}}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(w.home, "evidence.json")
	if err := os.WriteFile(path, evidence, 0600); err != nil {
		t.Fatal(err)
	}
	owner.must("owner", "adapter", "evidence", path)

	c := claudeSession{w: w, key: "reach-line-fixture", pid: strconv.Itoa(os.Getpid()), repo: repo}
	db := w.db()
	query := func(q string, args ...any) (v sql.NullInt64) {
		t.Helper()
		if err := db.QueryRow(q, args...).Scan(&v); err != nil && err != sql.ErrNoRows {
			t.Fatal(err)
		}
		return v
	}
	fire := func(event string, fields map[string]any) string {
		t.Helper()
		fields["hook_event_name"], fields["session_id"], fields["cwd"] = event, c.key, repo
		payload, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), binary, "hook", "claude", event)
		cmd.Dir, cmd.Stdin = repo, strings.NewReader(string(payload))
		cmd.Env = append(w.baseEnv(), "TMPDIR="+tmp, "CLAUDE_CODE_SESSION_ID="+c.key,
			"CLAUDE_PID="+c.pid, "CLAUDE_CODE_ENTRYPOINT=cli", "CLAUDE_CODE_MESSAGING_SOCKET="+socket,
			"AI_AGENT=claude-code_"+strings.ReplaceAll(version, ".", "-")+"_harness")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s hook: %v: %s", event, err, out)
		}
		return string(out)
	}
	route := func() string {
		var state, reason string
		_ = db.QueryRow(`SELECT state, reason FROM publish_route WHERE session_ref = ?`, c.key).Scan(&state, &reason)
		return state + " (" + reason + ")"
	}
	// until runs step, lets its hooks' packets be handled and checks done.
	until := func(what string, step func(), done func() bool) {
		t.Helper()
		deadline := time.Now().Add(fixtureEventTimeout)
		for {
			step()
			c.waitForConsumption(deadline)
			if done() {
				return
			}
			if time.Now().After(deadline) {
				w.logDispatcher()
				t.Fatalf("%s: route %s, last hook counter %q", what, route(), c.lastCounter())
			}
		}
	}
	turns := 0
	var promptCounter string
	// prompt runs one turn and returns what its prompt printed for the host.
	prompt := func() string {
		t.Helper()
		turns++
		turn := map[string]any{"prompt_id": fmt.Sprintf("reach-turn-%d", turns), "permission_mode": "default"}
		out := fire("UserPromptSubmit", turn)
		promptCounter = c.lastCounter()
		turn["stop_hook_active"] = false
		fire("Stop", turn)
		if out == "" {
			return ""
		}
		return hostContext(t, out)
	}
	told := func() sql.NullInt64 {
		return query(`SELECT b.reach_told FROM binding b JOIN publish_route r ON r.binding_id = b.id WHERE r.session_ref = ?`, c.key)
	}
	// tell checks that, once the dispatcher's tick has seen the change, the
	// very next prompt says line, and waits until the hook's print is stored.
	tell := func(line string, want int64) {
		t.Helper()
		until("the tick sees the change", func() { time.Sleep(25 * time.Millisecond) }, func() bool {
			return query(`SELECT b.reach_seen FROM binding b JOIN publish_route r ON r.binding_id = b.id WHERE r.session_ref = ?`,
				c.key) == sql.NullInt64{Int64: want, Valid: true}
		})
		for got := prompt(); !strings.Contains(got, line); got = prompt() {
			// Only a prompt whose hook ran out of time may miss it.
			if !strings.HasSuffix(promptCounter, " deadline") {
				t.Fatalf("the first prompt after the change printed %q (%s), want %q", got, promptCounter, line)
			}
		}
		until("told "+line, func() { prompt() }, func() bool { return told() == sql.NullInt64{Int64: want, Valid: true} })
	}
	// settled checks that the prompt after the one that said the line takes
	// the hook's clean path again: the stamp does not stay moved.
	settled := func() {
		t.Helper()
		if got := prompt(); got != "" || promptCounter != "UserPromptSubmit clean" {
			t.Fatalf("the prompt after the line printed %q (%s), want the clean path", got, promptCounter)
		}
	}

	until("start registers the route", func() {
		fire("SessionStart", map[string]any{"source": "startup", "permission_mode": "default"})
	}, func() bool {
		return query(`SELECT 1 FROM publish_route WHERE session_ref = ? AND socket_path = ?`, c.key, socket).Valid
	})
	until("route activated", func() { time.Sleep(25 * time.Millisecond) }, func() bool {
		return query(`SELECT 1 FROM publish_route_proof p JOIN publish_route r
			ON r.binding_id = p.binding_id AND r.generation = p.generation
			WHERE r.session_ref = ? AND p.activated_at_ms IS NOT NULL`, c.key).Valid
	})
	tell(domain.ReachBringsLine, 1)
	settled()

	// Each switch moves the stamp, so the next clean prompt asks once.
	member := &participant{w: w, extra: []string{"TMPDIR=" + tmp, "CLAUDE_CODE_SESSION_ID=" + c.key}}
	for _, flip := range []struct {
		name    string
		off, on func()
	}{
		{"owner hall switch", func() { owner.must("owner", "publish", "hall", "off") },
			func() { owner.must("owner", "publish", "hall", "on") }},
		{"member's own switch", func() { member.must("publish", "off") }, func() { member.must("publish", "on") }},
	} {
		flip.off()
		tell(domain.ReachLostLine, 0)
		settled()
		flip.on()
		tell(domain.ReachBringsLine, 1)
		settled()
	}

	// A change made while the dispatcher is down is told at the first
	// prompt after it starts.
	owner.must("owner", "stop")
	bindingID := query(`SELECT binding_id FROM publish_route WHERE session_ref = ?`, c.key).Int64
	writable, err := sql.Open("sqlite", "file:"+filepath.Join(w.state, "store.sqlite3")+"?mode=rw")
	if err != nil {
		t.Fatal(err)
	}
	_, err = writable.Exec(`UPDATE binding SET publish_owner = 0 WHERE id = ?`, bindingID)
	if err := errors.Join(err, writable.Close()); err != nil {
		t.Fatal(err)
	}
	owner.must("owner", "start")
	tell(domain.ReachLostLine, 0)
	settled()
	owner.must("owner", "publish", "member", strconv.FormatInt(bindingID, 10), "default")
	tell(domain.ReachBringsLine, 1)
	settled()
}
