//go:build conformance && darwin

package conformance

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
)

// The Antigravity CLI runs against Google's model with the owner's own
// sign-in, so these runs make billed model calls (about 12) and need the
// real home. They touch only a scratch workspace: crosshall's hooks are the
// workspace's .agents/hooks.json, its hall is the run's own state, and the
// owner's global hooks file and settings stay as they are.
const agyModel = "gemini-3.8-flash-medium"

const agyTurnEndCheck = "real Antigravity CLI turns: a request sent during a turn continues it once at its first eligible Stop, and the session reads and answers it"

type agyEvent struct {
	Event          string `json:"hookEventName"`
	InvocationNum  *int   `json:"invocationNum"`
	InitialSteps   *int   `json:"initialNumSteps"`
	ExecutionNum   *int   `json:"executionNum"`
	Termination    string `json:"terminationReason"`
	ConversationID string `json:"conversationId"`
}

// requireAgy installs the agy-2 hooks for the agy on PATH: setup writes
// them into the run's home, recording the agy build, and the workspace
// gets the same group beside a probe that logs every callback.
func (c *conformance) requireAgy() string {
	c.t.Helper()
	if os.Getenv("CROSSHALL_CONFORMANCE_AGY") != "1" {
		c.t.Skip("set CROSSHALL_CONFORMANCE_AGY=1 to run real, billed Antigravity turns")
	}
	agy, err := exec.LookPath("agy")
	if err != nil {
		c.t.Skip("no agy binary on PATH")
	}
	setupEnv := append(c.base()[1:], "HOME="+c.home)
	setupEnv[0] = "PATH=" + filepath.Dir(agy) + ":/usr/bin:/bin"
	config := filepath.Join(c.home, ".gemini", "config")
	if err := os.MkdirAll(config, 0700); err != nil {
		c.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(config, "hooks.json"), []byte("{}\n"), 0600); err != nil {
		c.t.Fatal(err)
	}
	cmd := exec.Command(c.crosshall, "setup", "antigravity", "--apply")
	cmd.Env, cmd.Dir = setupEnv, c.home
	if out, err := cmd.CombinedOutput(); err != nil || !strings.Contains(string(out), "agy: installed") {
		c.t.Fatalf("setup: %v\n%s", err, out)
	}
	var groups map[string]json.RawMessage
	raw, err := os.ReadFile(filepath.Join(config, "hooks.json"))
	if err != nil || json.Unmarshal(raw, &groups) != nil || groups["crosshall"] == nil {
		c.t.Fatalf("installed hooks: %s (%v)", raw, err)
	}
	probe := map[string][]map[string]any{}
	for _, event := range []string{"SessionStart", "PreInvocation", "PostInvocation", "Stop"} {
		probe[event] = []map[string]any{{"type": "command", "timeout": 5,
			"command": fmt.Sprintf("/bin/sh -c 'cat >> %s; printf \"\\n\" >> %s; printf \"{}\"'", c.agyLog(), c.agyLog())}}
	}
	probeJSON, _ := json.Marshal(probe)
	workspace := fmt.Sprintf("{\"crosshall\": %s, \"probe\": %s}\n", groups["crosshall"], probeJSON)
	if err := os.MkdirAll(filepath.Join(c.repo, ".agents"), 0700); err != nil {
		c.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.repo, ".agents", "hooks.json"), []byte(workspace), 0600); err != nil {
		c.t.Fatal(err)
	}
	return agy
}

func (c *conformance) agyLog() string { return filepath.Join(c.root, "agy-events.jsonl") }

func (c *conformance) agyEvents() []agyEvent {
	file, err := os.Open(c.agyLog())
	if err != nil {
		return nil
	}
	defer file.Close()
	var out []agyEvent
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1<<20), 1<<20)
	for scanner.Scan() {
		var e agyEvent
		if json.Unmarshal(scanner.Bytes(), &e) == nil && e.Event != "" {
			out = append(out, e)
		}
	}
	return out
}

func (c *conformance) agyCount(match func(agyEvent) bool) int {
	n := 0
	for _, e := range c.agyEvents() {
		if match(e) {
			n++
		}
	}
	return n
}

func finalStop(e agyEvent) bool { return e.Event == "Stop" }

// agyEnv is the owner's environment for agy (its sign-in lives in the real
// home) without any other agent's session variables, pointed at the run's
// hall and binary.
func (c *conformance) agyEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "CLAUDE") || strings.HasPrefix(name, "CODEX") || strings.HasPrefix(name, "ANTIGRAVITY") ||
			strings.HasPrefix(name, "CROSSHALL") || name == "AI_AGENT" || name == "PATH" {
			continue
		}
		env = append(env, kv)
	}
	return append(env, "PATH="+filepath.Dir(c.crosshall)+":"+os.Getenv("PATH"), "CROSSHALL_STATE_DIR="+c.state)
}

// startAgy opens an interactive agy on a pseudo-terminal in the workspace
// and accepts its trust question.
func (c *conformance) startAgy(agy string) *terminal {
	c.t.Helper()
	control, name, err := openPTY()
	if err != nil {
		c.t.Fatalf("open a pseudo-terminal: %v", err)
	}
	tty, err := os.OpenFile(name, os.O_RDWR, 0)
	if err != nil {
		c.t.Fatal(err)
	}
	defer tty.Close()
	size := struct{ rows, columns, x, y uint16 }{40, 120, 0, 0}
	if _, _, errno := syscall.Syscall(syscall.SYS_IOCTL, tty.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&size))); errno != 0 {
		c.t.Fatalf("size the pseudo-terminal: %v", errno)
	}
	cmd := exec.Command(agy, "--model", agyModel, "--dangerously-skip-permissions")
	cmd.Dir, cmd.Env = c.repo, append(c.agyEnv(), "TERM=xterm-256color")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	if err := cmd.Start(); err != nil {
		c.t.Fatal(err)
	}
	term := &terminal{c: c, control: control, cmd: cmd, exited: make(chan struct{})}
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := control.Read(buf)
			term.mu.Lock()
			term.screen.Write(buf[:n])
			term.wrote = time.Now()
			term.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	go func() { _ = cmd.Wait(); close(term.exited) }()
	c.t.Cleanup(func() {
		select {
		case <-term.exited:
		default:
			_ = cmd.Process.Kill()
			<-term.exited
		}
		control.Close()
	})
	term.awaitWithin(time.Minute, "agy's start screen", func() bool {
		text := term.text()
		return strings.Contains(text, "Do you trust") || strings.Contains(text, "for shortcuts")
	})
	if strings.Contains(term.text(), "Do you trust") {
		term.keys("\r")
		term.awaitWithin(time.Minute, "agy's prompt", func() bool { return strings.Contains(term.text(), "for shortcuts") })
	}
	term.idle()
	return term
}

// agyTurn submits a prompt and waits until the turn's last Stop is logged
// and the screen settles.
func (term *terminal) agyTurn(prompt string, stops int) {
	term.c.t.Helper()
	term.idle()
	before := term.c.agyCount(finalStop)
	term.keys(prompt)
	time.Sleep(300 * time.Millisecond)
	term.keys("\r")
	deadline := time.Now().Add(3 * time.Minute)
	for term.c.agyCount(finalStop) < before+stops {
		if time.Now().After(deadline) {
			var shape []string
			for _, e := range term.c.agyEvents() {
				shape = append(shape, fmt.Sprintf("%s(i=%v s=%v x=%v %s)", e.Event, deref(e.InvocationNum), deref(e.InitialSteps), deref(e.ExecutionNum), e.Termination))
			}
			screen := ansi.ReplaceAllString(term.text(), "")
			if len(screen) > 4000 {
				screen = screen[len(screen)-4000:]
			}
			term.c.t.Fatalf("no %d Stop(s) after %q; callbacks %v\nscreen:\n%s", stops, prompt, shape, screen)
		}
		time.Sleep(100 * time.Millisecond)
	}
	term.idle()
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;?<>]*[A-Za-z~]|\x1b\][^\x07]*\x07|\x1b[()][A-Z0-9]|\x1b[=>]`)

func deref(p *int) any {
	if p == nil {
		return "-"
	}
	return *p
}

// claudeWriter runs crosshall as a Claude Code session of another vendor,
// joined by hand.
var claudeWriterEnv = []string{"CLAUDE_CODE_SESSION_ID=0e1d2c3b-4a59-4876-8f9e-a0b1c2d3e4f5", "AI_AGENT=claude-code_2-1-331_agent"}

func (c *conformance) claudeWriter(args ...string) string {
	c.t.Helper()
	out, err := c.crosshallRun(claudeWriterEnv, args...)
	if err != nil {
		c.t.Fatalf("writer %v: %v\n%s", args, err, out)
	}
	return out
}

func (c *conformance) agyWriterSends(verb, body string) int64 {
	c.t.Helper()
	out := c.claudeWriter("--json", verb, "@agy1", body)
	var r struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil || r.ID == 0 {
		c.t.Fatalf("%s: %s", verb, out)
	}
	return r.ID
}

func (c *conformance) importAgyTurnEnd(version, level, check string) {
	c.t.Helper()
	evidence, _ := json.MarshalIndent(map[string]any{"vendor": "agy", "vendor_version": version,
		"platform": runtime.GOOS + "-" + runtime.GOARCH, "mode": "interactive", "manifest_rev": agyManifestRevision(),
		"run_at_ms": time.Now().UnixMilli(), "results": []evidenceResult{{"turn_end", level, check}}}, "", "  ")
	file := filepath.Join(c.root, fmt.Sprintf("agy-turn-end-%d.json", time.Now().UnixNano()))
	if err := os.WriteFile(file, append(evidence, '\n'), 0600); err != nil {
		c.t.Fatal(err)
	}
	if out := c.owner("owner", "adapter", "evidence", file); !strings.Contains(out, "Recorded 1 result(s)") {
		c.t.Fatalf("evidence import: %s", out)
	}
}

func agyManifestRevision() string {
	raw, err := manifests.Read("agy")
	if err != nil {
		return ""
	}
	var m struct {
		Revision string `json:"revision"`
	}
	_ = json.Unmarshal(raw, &m)
	return m.Revision
}

// agyVersion is the agy build setup recorded, as the hook reports it.
func (c *conformance) agyVersion(agy string) string {
	c.t.Helper()
	out, err := exec.Command(agy, "--version").Output()
	if err != nil {
		c.t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

const agyRules = "If a line starting with [crosshall] pending: appears, run `crosshall read` in your shell, then for each request it shows run `crosshall reply <id> \"done\"`; then answer me with the word ok."

func TestAntigravityInteractiveTurnEndConformance(t *testing.T) {
	world := func(t *testing.T) (*conformance, string) {
		t.Helper()
		c := newConformance(t)
		agy := c.requireAgy()
		c.owner("owner", "start")
		c.owner("owner", "publish", "hall", "off")
		c.owner("owner", "channel", "create", "checkout")
		c.owner("owner", "channel", "default", "checkout", "--repo", c.repo)
		c.owner("owner", "adapter", "set", "agy", "--enabled", "on")
		c.claudeWriter("join", "checkout", "--as", "writer")
		c.importAgyTurnEnd(c.agyVersion(agy), "tested", "bootstraps the exact profile this run measures")
		return c, agy
	}
	pid := `(SELECT principal_id FROM binding WHERE alias = 'agy1')`
	replied := func(c *conformance, id int64) bool {
		return c.storeCount(`SELECT COUNT(*) FROM message WHERE in_reply_to = ?`, id) > 0
	}
	// A request sent before a prompt reaches the turn's first model call;
	// the session reads and answers it.
	nextPrompt := func(t *testing.T) string {
		c, agy := world(t)
		term := c.startAgy(agy)
		term.agyTurn("Reply with just the word hello.", 1)
		id := c.agyWriterSends("ask", "please answer done")
		term.agyTurn("Reply with just the word again. "+agyRules, 1)
		if !replied(c, id) {
			t.Fatalf("request %d unanswered; screen:\n%s", id, term.text())
		}
		if n := c.storeCount(`SELECT COUNT(*) FROM recipient WHERE message_id = ? AND picked_up_at_ms IS NOT NULL AND principal_id = `+pid, id); n != 1 {
			t.Errorf("request %d not picked up by @agy1", id)
		}
		if n := c.storeCount(`SELECT COUNT(*) FROM turn_end_notice`); n != 0 {
			t.Errorf("%d turn-end claims for a request offered at the prompt", n)
		}
		return c.observedBuild("interactive")
	}
	// A request sent during a turn continues it once at its Stop.
	continued := func(t *testing.T) string {
		c, agy := world(t)
		term := c.startAgy(agy)
		term.agyTurn("Reply with just the word hello.", 1)
		turns := c.storeCount(`SELECT COUNT(*) FROM turn`)
		sent := make(chan sentAsk, 1)
		ctx := t.Context()
		go func() { sent <- c.askDuringNextTurn(ctx, turns) }()
		// agy runs no later Stop hook once one answers continue, so the
		// probe logs only the continued turn's own Stop.
		term.agyTurn("Count from one to five in words, one per line. "+agyRules, 1)
		var ask sentAsk
		select {
		case ask = <-sent:
		case <-time.After(2 * time.Minute):
			t.Fatal("the sender did not finish")
		}
		if ask.err != nil {
			t.Fatal(ask.err)
		}
		id := ask.id
		if !replied(c, id) {
			t.Fatalf("request %d unanswered; screen:\n%s", id, term.text())
		}
		if n := c.storeCount(`SELECT COUNT(*) FROM turn_end_notice`); n != 1 {
			t.Errorf("%d turn-end claims, want 1", n)
		}
		if n := c.storeCount(`SELECT COUNT(*) FROM announcement WHERE via = 'turn_end' AND message_id = ? AND principal_id = `+pid, id); n != 1 {
			t.Errorf("request %d claimed %d times at a Stop", id, n)
		}
		stops := 0
		for _, e := range c.agyEvents() {
			if e.Event == "Stop" && e.ExecutionNum != nil && *e.ExecutionNum > 0 {
				stops++
			}
		}
		if stops != 1 {
			t.Errorf("%d continued Stops, want 1", stops)
		}
		return c.observedBuild("interactive")
	}
	// The same prompt twice with nothing pending: two turns, no offer.
	repeated := func(t *testing.T) string {
		c, agy := world(t)
		term := c.startAgy(agy)
		term.agyTurn("Reply with just the word same.", 1)
		term.agyTurn("Reply with just the word same.", 1)
		if n := c.storeCount(`SELECT COUNT(*) FROM turn`); n < 2 {
			t.Errorf("%d turns for two prompts", n)
		}
		if n := c.storeCount(`SELECT COUNT(*) FROM turn_end_notice`); n != 0 {
			t.Errorf("%d turn-end claims with nothing pending", n)
		}
		return c.observedBuild("interactive")
	}
	run := &evidenceRun{t: t}
	run.run("next prompt", nextPrompt)
	run.run("continued", continued)
	run.run("repeated prompt", repeated)
	version, write := run.evidenceBuild(os.Getenv("CROSSHALL_CONFORMANCE_OUT") != "")
	if !write {
		return
	}
	c := &conformance{t: t}
	file := c.writeAgyEvidence(version)
	t.Logf("evidence: %s", file)
}

func (c *conformance) writeAgyEvidence(version string) string {
	c.t.Helper()
	evidence, _ := json.MarshalIndent(map[string]any{"vendor": "agy", "vendor_version": version,
		"platform": runtime.GOOS + "-" + runtime.GOARCH, "mode": "interactive", "manifest_rev": agyManifestRevision(),
		"run_at_ms": time.Now().UnixMilli(), "results": []evidenceResult{{"turn_end", "tested", agyTurnEndCheck}}}, "", "  ")
	dir := os.Getenv("CROSSHALL_CONFORMANCE_OUT")
	if err := safeOutputDir(dir); err != nil {
		c.t.Fatalf("CROSSHALL_CONFORMANCE_OUT: %v", err)
	}
	file := filepath.Join(dir, fmt.Sprintf("agy-%s-%s-%s-interactive-turn-end.json", version, runtime.GOOS, runtime.GOARCH))
	out, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := out.Write(append(evidence, '\n')); err != nil || out.Close() != nil {
		c.t.Fatal("cannot write the evidence file")
	}
	return file
}

type sentAsk struct {
	id  int64
	err error
}

// askDuringNextTurn sends the request once the store has more turns than
// before, that is once the next prompt opened its turn. It runs beside the
// test, so it reports instead of failing, and gives up when the test ends.
func (c *conformance) askDuringNextTurn(ctx context.Context, before int) sentAsk {
	deadline := time.Now().Add(time.Minute)
	for {
		db, err := sql.Open("sqlite", "file:"+filepath.Join(c.state, "store.sqlite3")+"?mode=ro&_pragma=busy_timeout(5000)")
		if err != nil {
			return sentAsk{err: err}
		}
		var n int
		err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM turn`).Scan(&n)
		db.Close()
		switch {
		case ctx.Err() != nil:
			return sentAsk{err: ctx.Err()}
		case err != nil:
			return sentAsk{err: err}
		case n > before:
			out, err := c.crosshallRun(claudeWriterEnv, "--json", "ask", "@agy1", "please answer done")
			var r struct {
				ID int64 `json:"id"`
			}
			if err != nil || json.Unmarshal([]byte(out), &r) != nil || r.ID == 0 {
				return sentAsk{err: fmt.Errorf("ask: %v: %s", err, out)}
			}
			return sentAsk{id: r.ID}
		case time.Now().After(deadline):
			return sentAsk{err: fmt.Errorf("no new turn within a minute; nothing sent")}
		}
		time.Sleep(20 * time.Millisecond)
	}
}
