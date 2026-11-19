//go:build conformance

package conformance

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// turnEndWorld is one disposable world for the turn-end check: a joined
// Claude Code session in print mode, a shell-only writer with a root token,
// and, when tested is set, a turn_end row for this exact build and mode.
func turnEndWorld(t *testing.T, tested bool) (*conformance, string) {
	t.Helper()
	c := newConformance(t)
	c.requireClaude()
	c.owner("owner", "start")
	c.owner("owner", "channel", "create", "checkout")
	c.owner("owner", "channel", "default", "checkout", "--repo", c.repo)
	c.owner("owner", "adapter", "set", "claude", "--enabled", "on")
	c.writer("join", "checkout", "--as", "writer")
	c.owner("owner", "budget", "grant", "checkout/@writer", "--roots", "1")
	c.install()
	version := c.version()
	if tested {
		c.importTurnEnd(version, "tested", "loopback run before the check: bootstraps the exact profile it measures")
	}
	return c, version
}

// importTurnEnd records one turn_end result for this build in print mode.
func (c *conformance) importTurnEnd(version, level, check string) string {
	c.t.Helper()
	evidence, _ := json.MarshalIndent(map[string]any{"vendor": "claude", "vendor_version": version,
		"platform": runtime.GOOS + "-" + runtime.GOARCH, "mode": "print", "manifest_rev": "claude-1",
		"run_at_ms": time.Now().UnixMilli(), "results": []evidenceResult{{"turn_end", level, check}}}, "", "  ")
	file := filepath.Join(c.root, fmt.Sprintf("turn-end-%d.json", time.Now().UnixNano()))
	if err := os.WriteFile(file, append(evidence, '\n'), 0600); err != nil {
		c.t.Fatal(err)
	}
	if out := c.owner("owner", "adapter", "evidence", file); !strings.Contains(out, "Recorded 1 result(s)") {
		c.t.Fatalf("evidence import: %s", out)
	}
	return string(evidence)
}

// writerSends sends a message to the session's member and returns its ID.
func (c *conformance) writerSends(verb, body string) int64 {
	c.t.Helper()
	out := c.writer("--json", verb, "@claude1", body)
	var r struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil || r.ID == 0 {
		c.t.Fatalf("%s: %s", verb, out)
	}
	return r.ID
}

// storeCount runs one count against the disposable store.
func (c *conformance) storeCount(query string, args ...any) int {
	c.t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(c.state, "store.sqlite3")+"?mode=ro")
	if err != nil {
		c.t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		c.t.Fatal(err)
	}
	return n
}

// committedClaims is the doctor's turn-end claim count for Claude Code today.
func (c *conformance) committedClaims() int {
	c.t.Helper()
	out := c.owner("--json", "owner", "doctor")
	var r struct {
		Adapters []struct {
			Adapter       string `json:"adapter"`
			TurnEndNotice struct {
				CommittedClaims int `json:"committed_claims"`
			} `json:"turn_end_notice"`
		} `json:"adapters"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		c.t.Fatalf("doctor: %v\n%s", err, out)
	}
	for _, a := range r.Adapters {
		if a.Adapter == "claude" {
			return a.TurnEndNotice.CommittedClaims
		}
	}
	c.t.Fatalf("doctor has no claude adapter: %s", out)
	return 0
}

const stopContext = "Stop hook additional context: [crosshall] pending: "

// carries counts the Stop context for id in the model request n.
func (c *conformance) carries(n int, id int64) int {
	return strings.Count(c.model.request(n), stopContext+fmt.Sprint(id))
}

// The turn-end rule as Claude Code's adapter checks it, in print mode against
// the loopback model. A request stored while the turn is with the model is
// still unread when the turn ends: with tested evidence for this build the
// Stop continues the turn once with the pending line and nothing else; the
// next Stop adds nothing. Without evidence, for a note, or for a request the
// member already read, the turn ends as it did before.
func TestClaudeCodeTurnEndConformance(t *testing.T) {
	var version string
	gated := func(t *testing.T) {
		c, _ := turnEndWorld(t, false)
		var id int64
		c.model.during[1] = func() { id = c.writerSends("ask", "synthetic request during the turn") }
		c.claudeRun("first synthetic prompt")
		if c.model.count() != 1 || c.carries(1, id) != 0 || c.storeCount(`SELECT COUNT(*) FROM turn_end_notice`) != 0 {
			t.Fatalf("without evidence: %d requests, %d claims", c.model.count(), c.storeCount(`SELECT COUNT(*) FROM turn_end_notice`))
		}
	}
	continued := func(t *testing.T) {
		c, v := turnEndWorld(t, true)
		version = v
		var id int64
		c.model.during[1] = func() { id = c.writerSends("ask", "synthetic request during the turn") }
		c.claudeRun("first synthetic prompt")
		logRequestTokenSamples(t, "claude", []byte(c.model.request(1)), "hint")
		logRequestTokenSamples(t, "claude", []byte(c.model.request(2)), "pending")
		if c.model.count() != 2 {
			t.Fatalf("%d model requests, want the turn and one continuation", c.model.count())
		}
		if c.carries(1, id) != 0 || c.carries(2, id) != 1 {
			t.Fatalf("pending line in requests 1 and 2: %d, %d", c.carries(1, id), c.carries(2, id))
		}
		if !strings.Contains(c.model.request(1), "Before you end a turn by your own choice, run `crosshall inbox`") {
			t.Error("the join hint in the session's first request lacks the turn-end rule")
		}
		pid := `(SELECT principal_id FROM binding WHERE alias = 'claude1')`
		if n := c.storeCount(`SELECT COUNT(*) FROM turn_end_notice`); n != 1 {
			t.Errorf("%d turn claims, want 1", n)
		}
		if n := c.storeCount(`SELECT COUNT(*) FROM turn_end_notice_item WHERE message_id = ? AND principal_id = `+pid, id); n != 1 {
			t.Errorf("request %d claimed %d times", id, n)
		}
		if n := c.storeCount(`SELECT COUNT(*) FROM recipient WHERE message_id = ? AND picked_up_at_ms IS NULL AND principal_id = `+pid, id); n != 1 {
			t.Error("the continuation picked the request up; it must stay unread in the inbox")
		}
		if n := c.committedClaims(); n != 1 {
			t.Errorf("doctor counts %d claims, want 1", n)
		}
	}
	note := func(t *testing.T) {
		c, _ := turnEndWorld(t, true)
		c.model.during[1] = func() { c.writerSends("note", "synthetic note during the turn") }
		c.claudeRun("first synthetic prompt")
		if c.model.count() != 1 || strings.Contains(c.model.request(1), stopContext) || c.storeCount(`SELECT COUNT(*) FROM turn_end_notice`) != 0 {
			t.Fatalf("a note continued the turn: %d requests", c.model.count())
		}
	}
	alreadyRead := func(t *testing.T) {
		c, _ := turnEndWorld(t, true)
		var id int64
		c.model.during[1] = func() { id = c.writerSends("ask", "synthetic request read during the turn") }
		c.model.bash[1] = c.crosshall + " read"
		cmd := exec.Command(c.claude, "-p", "--model", "haiku", "--allowedTools", "Bash")
		cmd.Dir, cmd.Env, cmd.Stdin = c.repo, c.claudeEnv(), strings.NewReader("first synthetic prompt")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("claude: %v\n%s", err, out)
		}
		pid := `(SELECT principal_id FROM binding WHERE alias = 'claude1')`
		if n := c.storeCount(`SELECT COUNT(*) FROM recipient WHERE message_id = ? AND picked_up_at_ms IS NOT NULL AND principal_id = `+pid, id); n != 1 {
			t.Fatalf("the session's read did not pick request %d up", id)
		}
		if c.model.count() != 2 || c.carries(2, id) != 0 || c.storeCount(`SELECT COUNT(*) FROM turn_end_notice`) != 0 {
			t.Fatalf("a request already read continued the turn: %d requests", c.model.count())
		}
	}
	for name, run := range map[string]func(*testing.T){"no evidence": gated, "tested": continued, "note": note, "already read": alreadyRead} {
		if !t.Run(name, run) {
			t.Fatalf("%s: no turn_end evidence is written", name)
		}
	}

	// Every check held for this build and mode: write the evidence row.
	dir := os.Getenv("CROSSHALL_CONFORMANCE_OUT")
	if dir == "" {
		return
	}
	if err := safeOutputDir(dir); err != nil {
		t.Fatalf("CROSSHALL_CONFORMANCE_OUT: %v", err)
	}
	evidence, _ := json.MarshalIndent(map[string]any{"vendor": "claude", "vendor_version": version,
		"platform": runtime.GOOS + "-" + runtime.GOARCH, "mode": "print", "manifest_rev": "claude-1",
		"run_at_ms": time.Now().UnixMilli(), "results": []evidenceResult{{"turn_end", "tested",
			"an unread request stored during the turn continues it once with the pending line; no evidence, a note or a read request does not"}}}, "", "  ")
	file := filepath.Join(dir, fmt.Sprintf("claude-%s-%s-%s-print-turn-end.json", version, runtime.GOOS, runtime.GOARCH))
	out, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := out.Write(append(evidence, '\n')); err != nil || out.Close() != nil {
		t.Fatal("cannot write the evidence file")
	}
	t.Logf("evidence: %s\n%s", file, evidence)
}
