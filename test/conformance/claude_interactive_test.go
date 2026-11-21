//go:build conformance

package conformance

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

// interactiveWait bounds every wait for something an interactive session
// does. Waits end on the observed event, not on this time.
const interactiveWait = 30 * time.Second

// hookEvent is one hook callback of the session as Claude Code sent it.
type hookEvent struct {
	Name       string `json:"hook_event_name"`
	Session    string `json:"session_id"`
	Prompt     string `json:"prompt_id"`
	Source     string `json:"source"`
	Reason     string `json:"reason"`
	Notice     string `json:"notification_type"`
	StopActive bool   `json:"stop_hook_active"`
	Process    string `json:"-"` // CLAUDE_PID in the hook's environment
}

// terminal is one interactive Claude Code session on a pseudo-terminal.
type terminal struct {
	c       *conformance
	control *os.File
	cmd     *exec.Cmd
	exited  chan struct{}
	mu      sync.Mutex
	screen  bytes.Buffer
	wrote   time.Time // when the session last wrote to its terminal
}

// eventLog is where the probe hook appends every callback's environment and payload.
func (c *conformance) eventLog() string { return filepath.Join(c.root, "events.log") }

// writeProbe writes the hook command that logs a callback and returns its
// path. Callbacks can run at the same time, so they share nothing but the
// log: each writes its payload to a file of its own and then names that file
// in one short line, which a single append writes whole. A lock made with
// mkdir is no alternative: the mkdir of Ubuntu 26.04 reports success to two
// callers at once.
func (c *conformance) writeProbe() string {
	c.t.Helper()
	records := c.eventLog() + ".d"
	if err := os.Mkdir(records, 0700); err != nil {
		c.t.Fatal(err)
	}
	probe := filepath.Join(c.root, "events.sh")
	script := "#!/bin/sh\nrecord=$(mktemp '" + records + "/XXXXXXXX') && cat > \"$record\" &&\n" +
		"\tprintf '%s\\t%s\\n' \"$CLAUDE_PID\" \"$record\" >> '" + c.eventLog() + "'\n"
	if err := os.WriteFile(probe, []byte(script), 0700); err != nil {
		c.t.Fatal(err)
	}
	return probe
}

// installInteractive installs the adapter as install does, adds a probe to
// every event that logs its payload, and prepares the scratch configuration
// so that an interactive session starts without a first-run dialog: the
// onboarding is done, the fixture key is approved and the checkout is trusted.
func (c *conformance) installInteractive() {
	c.t.Helper()
	c.install()
	path := filepath.Join(c.config, "settings.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		c.t.Fatal(err)
	}
	var settings struct {
		Hooks map[string][]any `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &settings); err != nil {
		c.t.Fatal(err)
	}
	probe := c.writeProbe()
	for event := range settings.Hooks {
		settings.Hooks[event] = append(settings.Hooks[event],
			map[string]any{"hooks": []any{map[string]any{"type": "command", "command": probe, "timeout": 5}}})
	}
	if raw, err = json.MarshalIndent(settings, "", "  "); err != nil {
		c.t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		c.t.Fatal(err)
	}
	repo, err := filepath.EvalSymlinks(c.repo)
	if err != nil {
		c.t.Fatal(err)
	}
	const key = "sk-ant-fixture-not-a-key" // the key claudeEnv sets
	global, _ := json.Marshal(map[string]any{
		"hasCompletedOnboarding": true, "theme": "dark",
		"customApiKeyResponses": map[string]any{"approved": []string{key[len(key)-20:]}, "rejected": []string{}},
		"projects":              map[string]any{repo: map[string]any{"hasTrustDialogAccepted": true, "hasCompletedProjectOnboarding": true}},
	})
	if err := os.WriteFile(filepath.Join(c.config, ".claude.json"), global, 0600); err != nil {
		c.t.Fatal(err)
	}
}

// events reads the callbacks logged so far, in the order of the log. A line
// that is still being written has no newline yet and is left for the next
// read; a complete line whose record does not parse fails the test, so a
// lost callback cannot pass for one that never came.
func (c *conformance) events() []hookEvent {
	raw, _ := os.ReadFile(c.eventLog())
	complete := string(raw[:bytes.LastIndexByte(raw, '\n')+1])
	var out []hookEvent
	for _, line := range strings.Split(strings.TrimSuffix(complete, "\n"), "\n") {
		if complete == "" {
			break
		}
		process, record, ok := strings.Cut(line, "\t")
		payload, err := os.ReadFile(record)
		var e hookEvent
		if !ok || err != nil || json.Unmarshal(payload, &e) != nil || e.Name == "" {
			c.malformed.Do(func() { c.t.Errorf("malformed callback record: %q (%v)", line, err) })
			continue
		}
		e.Process = process
		out = append(out, e)
	}
	return out
}

// inSession keeps one session's callbacks.
func inSession(events []hookEvent, session string) []hookEvent {
	var out []hookEvent
	for _, e := range events {
		if e.Session == session {
			out = append(out, e)
		}
	}
	return out
}

// turnShape renders the prompt and Stop callbacks in order, naming each
// prompt ID by a letter in order of first appearance. A callback without a
// prompt ID gets "?" and no letter, so it matches no expected shape.
func turnShape(events []hookEvent) string {
	var shape []string
	letters := map[string]string{}
	for _, e := range events {
		if e.Name != "UserPromptSubmit" && e.Name != "Stop" {
			continue
		}
		name, letter := e.Name, "?"
		if e.StopActive {
			name = "ContinuedStop"
		}
		if e.Prompt != "" {
			if letters[e.Prompt] == "" {
				letters[e.Prompt] = string(rune('A' + len(letters)))
			}
			letter = letters[e.Prompt]
		}
		shape = append(shape, name+" "+letter)
	}
	return strings.Join(shape, ", ")
}

// idleTurns is the shape of n turns typed at an idle prompt: each prompt is
// followed by its own Stop, under an ID no other turn has.
func idleTurns(n int) string {
	var shape []string
	for i := range n {
		letter := string(rune('A' + i))
		shape = append(shape, "UserPromptSubmit "+letter, "Stop "+letter)
	}
	return strings.Join(shape, ", ")
}

// interactive starts `claude` on a pseudo-terminal in the temporary homes
// against the loopback endpoint, in the default permission mode, and waits
// for the callback of its start.
func (c *conformance) interactive(args ...string) *terminal {
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
	cmd := exec.Command(c.claude, append([]string{"--model", "haiku", "--permission-mode", "default"}, args...)...)
	cmd.Dir, cmd.Env = c.repo, append(c.claudeEnv(), "TERM=xterm-256color")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	started := len(c.events())
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
	term.await("the session's start callback and its prompt", func() bool {
		events := c.events()
		return len(events) > started && events[len(events)-1].Name == "SessionStart" && strings.Contains(term.text(), "❯")
	})
	return term
}

// text is what the session has written to its terminal so far.
func (term *terminal) text() string {
	term.mu.Lock()
	defer term.mu.Unlock()
	return term.screen.String()
}

// await polls until done holds, and fails the test with the terminal's last
// output when it does not within interactiveWait.
func (term *terminal) await(what string, done func() bool) {
	term.c.t.Helper()
	for deadline := time.Now().Add(interactiveWait); !done(); time.Sleep(20 * time.Millisecond) {
		if time.Now().After(deadline) {
			screen := term.text()
			if len(screen) > 1500 {
				screen = screen[len(screen)-1500:]
			}
			term.c.t.Fatalf("no %s within %s; events %+v\nterminal: %q", what, interactiveWait, term.c.events(), screen)
		}
	}
}

// idleQuiet is how long the terminal must stay silent to count as idle. A
// running turn or hook redraws its status several times in that time.
const idleQuiet = 600 * time.Millisecond

// idle waits until the session has stopped drawing, so that the next line is
// typed at an idle prompt. A line typed earlier is typed ahead: Claude Code
// then reports the previous turn's prompt ID for it
// (TestClaudeCodeInteractiveTypedAheadPrompt).
func (term *terminal) idle() {
	term.c.t.Helper()
	term.await("an idle prompt", func() bool {
		term.mu.Lock()
		defer term.mu.Unlock()
		return time.Since(term.wrote) > idleQuiet
	})
}

// keys types into the session's terminal.
func (term *terminal) keys(text string) {
	term.c.t.Helper()
	if _, err := term.control.WriteString(text); err != nil {
		term.c.t.Fatalf("type into the session: %v", err)
	}
}

// count is the number of logged callbacks that match.
func (c *conformance) count(match func(hookEvent) bool) int {
	n := 0
	for _, e := range c.events() {
		if match(e) {
			n++
		}
	}
	return n
}

func named(name string) func(hookEvent) bool {
	return func(e hookEvent) bool { return e.Name == name }
}

// submit types one line and Enter at an idle prompt, then waits for one more
// callback that matches.
func (term *terminal) submit(line string, match func(hookEvent) bool, what string) {
	term.c.t.Helper()
	term.idle()
	before := term.c.count(match)
	term.keys(line + "\r")
	term.await(what, func() bool { return term.c.count(match) > before })
}

// turn submits a prompt and waits for the Stop that ends its turn.
func (term *terminal) turn(prompt string) {
	term.c.t.Helper()
	term.submit(prompt, func(e hookEvent) bool { return e.Name == "Stop" && !e.StopActive }, "Stop after "+prompt)
}

// exit ends the session with /exit and waits for its process.
func (term *terminal) exit() {
	term.c.t.Helper()
	term.submit("/exit", named("SessionEnd"), "SessionEnd after /exit")
	select {
	case <-term.exited:
	case <-time.After(interactiveWait):
		term.c.t.Fatal("the session's process did not end after /exit")
	}
}

// writeEvidence writes one run's results for this build and mode to the
// evidence directory (CROSSHALL_CONFORMANCE_OUT, or the run's own root) and
// returns the file.
func (c *conformance) writeEvidence(name, version, mode string, results []evidenceResult) string {
	c.t.Helper()
	evidence, _ := json.MarshalIndent(map[string]any{"vendor": "claude", "vendor_version": version,
		"platform": runtime.GOOS + "-" + runtime.GOARCH, "mode": mode, "manifest_rev": "claude-2",
		"run_at_ms": time.Now().UnixMilli(), "results": results}, "", "  ")
	dir := os.Getenv("CROSSHALL_CONFORMANCE_OUT")
	if dir == "" {
		dir = c.root
	} else if err := safeOutputDir(dir); err != nil {
		c.t.Fatalf("CROSSHALL_CONFORMANCE_OUT: %v", err)
	}
	file := filepath.Join(dir, fmt.Sprintf("claude-%s-%s-%s-%s.json", version, runtime.GOOS, runtime.GOARCH, name))
	out, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := out.Write(append(evidence, '\n')); err != nil || out.Close() != nil {
		c.t.Fatal("cannot write the evidence file")
	}
	c.t.Logf("evidence: %s\n%s", file, evidence)
	return file
}

// Claude Code as most people run it: interactively, on a terminal. The run
// drives a real session through the installed adapter against the loopback
// model and observes what print mode's run observes, plus the lifecycle only
// an interactive session has: an approval dialog, /compact, /clear, /exit and
// a resumed session.
func TestClaudeCodeInteractiveConformance(t *testing.T) {
	c := newConformance(t)
	c.requireClaude()
	c.owner("owner", "start")
	c.owner("owner", "channel", "create", "checkout")
	c.owner("owner", "channel", "default", "checkout", "--repo", c.repo)
	c.owner("owner", "adapter", "set", "claude", "--enabled", "on")
	c.writer("join", "checkout", "--as", "writer")
	c.installInteractive()
	version := c.version()
	observed := func() string {
		var r struct {
			Members []struct {
				Alias    string `json:"alias"`
				Observed *struct {
					Fact string `json:"fact"`
				} `json:"observed"`
			} `json:"members"`
		}
		_ = json.Unmarshal([]byte(c.writer("--json", "who")), &r)
		for _, m := range r.Members {
			if strings.HasPrefix(m.Alias, "claude") && m.Observed != nil {
				return m.Observed.Fact
			}
		}
		return ""
	}
	// A hook publishes an observation without waiting for the dispatcher, so
	// another member sees the fact a moment later: sees waits for it while the
	// session stays where it is (with the model, or at its dialog).
	sees := func(fact string) string {
		var got string
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			if got = observed(); got == fact {
				break
			}
		}
		return got
	}
	joined := func() bool { return regexp.MustCompile(`@claude[0-9]`).MatchString(c.writer("who")) }

	// Three prompts as in print mode. Mail arrives while the first turn is
	// with the model, so it is due at the second prompt; the third is quiet.
	var noteID, promptFact string
	c.model.during[1] = func() {
		var r struct {
			ID int64 `json:"id"`
		}
		_ = json.Unmarshal([]byte(c.writer("--json", "note", "@claude1", "synthetic conformance note")), &r)
		noteID = fmt.Sprint(r.ID)
	}
	c.model.during[3] = func() { promptFact = sees("prompt_submitted") }
	term := c.interactive()
	term.turn("first synthetic prompt")
	term.turn("second synthetic prompt")
	term.turn("third synthetic prompt")
	if c.model.count() != 3 || noteID == "" {
		t.Fatalf("%d model requests, note %q", c.model.count(), noteID)
	}
	pending := "[crosshall] pending: " + noteID
	enrolled := strings.Contains(c.model.request(1), "Joined #checkout as @claude1")
	offered := !strings.Contains(c.model.request(1), pending) && strings.Count(c.model.request(2), pending) == 1 &&
		strings.Count(c.model.request(3), pending) == 1
	quiet := strings.Count(c.model.request(3), "[crosshall]") == strings.Count(c.model.request(2), "[crosshall]")

	// A tool call that needs approval: the dialog is announced, and after
	// the answer the turn goes on to its Stop.
	approved := filepath.Join(c.root, "approved")
	c.model.bash[4] = "touch " + approved
	term.submit("fourth synthetic prompt", func(e hookEvent) bool {
		return e.Name == "Notification" && e.Notice == "permission_prompt"
	}, "the approval dialog's callback")
	approvalFact := sees("approval_requested")
	stops := c.count(named("Stop"))
	term.idle()
	term.keys("\r") // the dialog's first choice allows the call once
	term.await("the Stop of the approved turn", func() bool { return c.count(named("Stop")) > stops })
	_, err := os.Stat(approved)
	approval := approvalFact == "approval_requested" && err == nil

	// member is the principal a session ID is the live key of, and that
	// principal's live binding; zero where there is none.
	member := func(session string) (principal, binding int) {
		principal = c.storeCount(`SELECT COALESCE(MAX(principal_id), 0) FROM principal_key WHERE vendor = 'claude' AND key = ? AND state = 'live'`, session)
		binding = c.storeCount(`SELECT COALESCE(MAX(id), 0) FROM binding WHERE principal_id = ? AND state = 'live' AND alias = 'claude1'`, principal)
		return principal, binding
	}

	// /compact keeps the session; /clear starts a new one in the same process.
	first := c.events()[0]
	principal, binding := member(first.Session)
	term.submit("/compact", func(e hookEvent) bool { return e.Name == "SessionStart" && e.Source == "compact" }, "the start after /compact")
	compacted := c.events()[len(c.events())-1]
	term.turn("fifth synthetic prompt")
	term.submit("/clear", func(e hookEvent) bool { return e.Name == "SessionStart" && e.Source == "clear" }, "the start after /clear")
	all := c.events()
	cleared, ended := all[len(all)-1], all[len(all)-2]
	term.turn("sixth synthetic prompt")
	// The cleared conversation is the member the session was: its new ID is
	// the live key of the same principal, which keeps its binding, and the
	// old ID is no longer a live key.
	clearedPrincipal, clearedBinding := member(cleared.Session)
	oldPrincipal, _ := member(first.Session)
	kept := principal != 0 && binding != 0 && clearedPrincipal == principal && clearedBinding == binding && oldPrincipal == 0
	lifecycle := first.Source == "startup" && compacted.Session == first.Session &&
		ended.Name == "SessionEnd" && ended.Reason == "clear" && ended.Session == first.Session &&
		cleared.Session != first.Session && cleared.Process == first.Process && first.Process != "" && kept

	// /exit is a clean end: the member departs. Resuming restores it.
	term.exit()
	all = c.events()
	exit := all[len(all)-1]
	departed := exit.Reason == "prompt_input_exit" && !joined()
	resumed := c.interactive("--resume", cleared.Session)
	all = c.events()
	resumedPrincipal, resumedBinding := member(cleared.Session)
	restored := all[len(all)-1].Source == "resume" && all[len(all)-1].Session == cleared.Session &&
		resumedPrincipal == principal && resumedBinding == binding
	resumed.exit()

	// Typed at an idle prompt, every prompt opens a turn of its own: in
	// each session, a prompt and then its Stop under one ID that no other
	// turn has. Five turns ran before /clear and one after it.
	all = c.events()
	ownTurns := first.Session != "" && cleared.Session != "" && turnShape(all) == idleTurns(6) &&
		turnShape(inSession(all, first.Session)) == idleTurns(5) && turnShape(inSession(all, cleared.Session)) == idleTurns(1)

	probe, _ := os.ReadFile(filepath.Join(c.root, "probe.env"))
	t.Logf("claude %s; hook environment: %s", version, strings.ReplaceAll(strings.TrimSpace(string(probe)), "\n", ", "))
	for _, e := range c.events() {
		t.Logf("callback %-16s session %.8s prompt %.8s source %q reason %q notice %q continued %t", e.Name, e.Session, e.Prompt, e.Source, e.Reason, e.Notice, e.StopActive)
	}
	// The build and the mode come from the launching process: the scratch
	// environment sets neither label.
	launchVersion := strings.Contains(string(probe), "claude-code_"+strings.ReplaceAll(version, ".", "-"))
	profiles := c.launchProfiles()
	launchProfile := len(profiles) > 0
	for _, p := range profiles {
		launchProfile = launchProfile && p.VendorVersion == version && p.Mode == "interactive"
	}
	provenance := launchVersion && launchProfile
	level := func(ok bool) string {
		if ok && provenance {
			return "tested"
		}
		return "candidate"
	}
	results := []evidenceResult{
		{"enroll", level(enrolled && lifecycle && departed && restored),
			"start line at start; /compact keeps the session; /clear starts a new one in the same process, still the same member; /exit departs; resume restores"},
		{"boundary", level(offered && quiet && ownTurns),
			"pending line once at the prompt after the mail; quiet turn adds nothing; prompts typed at an idle prompt, each a turn of its own"},
		{"observe", level(promptFact == "prompt_submitted" && approval && departed),
			"prompt and approval facts visible to another member; the approved turn reaches its Stop; end departs"},
	}
	for name, ok := range map[string]bool{"enrolled": enrolled, "offered": offered, "quiet": quiet, "approval": approval,
		"own turns": ownTurns, "lifecycle": lifecycle, "departed": departed, "restored": restored, "launch version": launchVersion, "launch profile": launchProfile} {
		if !ok {
			t.Errorf("%s: not observed", name)
		}
	}
	if promptFact != "prompt_submitted" {
		t.Errorf("observed fact %q", promptFact)
	}
	if t.Failed() {
		t.Fatal("a check failed: no evidence is written or recorded")
	}
	file := c.writeEvidence("interactive", version, "interactive", results)
	if out := c.owner("owner", "adapter", "evidence", file); !strings.Contains(out, "Recorded 3 result(s)") {
		t.Fatalf("evidence import: %s", out)
	}
	recorded := c.evidenceLevels(version, runtime.GOOS+"-"+runtime.GOARCH, "interactive", "claude-2")
	for _, r := range results {
		if recorded[r.Cap] != r.Level {
			t.Errorf("imported %s %q, exported %q", r.Cap, recorded[r.Cap], r.Level)
		}
	}
}

// typedAheadShape is what Claude Code reports for a turn and a prompt typed
// while that turn runs.
const typedAheadShape = "UserPromptSubmit A, UserPromptSubmit A, Stop A, Stop B"

// A prompt typed while the previous turn has not ended (typed ahead, the way
// people queue the next thing) is reported with the previous turn's prompt
// ID, before that turn's Stop, and its own turn ends with a Stop whose ID no
// prompt ever carried. This pins that shape for the build under test: the
// adapter's handling of a typed-ahead prompt as a turn of its own rests on
// it.
func TestClaudeCodeInteractiveTypedAheadPrompt(t *testing.T) {
	c := newConformance(t)
	c.requireClaude()
	c.owner("owner", "start")
	c.owner("owner", "channel", "create", "checkout")
	c.owner("owner", "channel", "default", "checkout", "--repo", c.repo)
	c.owner("owner", "adapter", "set", "claude", "--enabled", "on")
	c.writer("join", "checkout", "--as", "writer")
	c.installInteractive()
	term := c.interactive()
	c.model.during[1] = func() {
		prompts := c.count(named("UserPromptSubmit"))
		term.keys("second synthetic prompt\r")
		term.await("the typed-ahead prompt's callback", func() bool { return c.count(named("UserPromptSubmit")) > prompts })
	}
	term.keys("first synthetic prompt\r")
	term.await("the Stop of both turns", func() bool { return c.count(named("Stop")) == 2 })
	all := c.events()
	session := all[0].Session
	got := turnShape(inSession(all, session))
	t.Logf("callbacks: %s; %d model requests", got, c.model.count())
	if session == "" || turnShape(all) != got {
		t.Fatalf("callbacks of more than one session, or of none: %q", turnShape(all))
	}
	if got != typedAheadShape || c.model.count() != 2 {
		want := typedAheadShape
		t.Fatalf("typed-ahead prompt: callbacks %q with %d model requests, want %q with 2: the shape the adapter's typed-ahead handling rests on has changed", got, c.model.count(), want)
	}
	term.exit()
}

// A publish notice that crosshall submits while a turn is with the model does
// not take the typed-ahead shape: Claude Code runs it after that turn's Stop
// as a turn of its own, with a prompt ID of its own, and the hall observes it
// as its own turn. So does a notice to an idle session. The scratch hall
// lists this build for publish only so that the sender runs; the run records
// facts and writes no evidence, which for publish takes a run
// a person watches (CONFORMANCE.md).
func TestClaudeCodeInteractivePublishNoticeDuringATurn(t *testing.T) {
	c := newConformance(t)
	c.requireClaude()
	c.owner("owner", "start")
	c.owner("owner", "channel", "create", "checkout")
	c.owner("owner", "channel", "default", "checkout", "--repo", c.repo)
	c.owner("owner", "adapter", "set", "claude", "--enabled", "on")
	c.writer("join", "checkout", "--as", "writer")
	c.owner("owner", "budget", "grant", "checkout/@writer", "--roots", "2")
	c.installInteractive()
	listed, _ := json.Marshal(map[string]any{"vendor": "claude", "vendor_version": c.version(),
		"platform": runtime.GOOS + "-" + runtime.GOARCH, "mode": "interactive", "manifest_rev": "claude-2",
		"run_at_ms": time.Now().UnixMilli(), "results": []evidenceResult{{"publish", "tested", "scratch hall only: lets the harness observe a notice"}}})
	file := filepath.Join(c.root, "publish.json")
	if err := os.WriteFile(file, listed, 0600); err != nil {
		t.Fatal(err)
	}
	c.owner("owner", "adapter", "evidence", file)
	term := c.interactive()
	term.turn("first synthetic prompt")

	// To an idle session.
	stops := c.count(named("Stop"))
	idle := c.writerSends("ask", "synthetic request while idle")
	term.await("the Stop of the idle notice's turn", func() bool { return c.count(named("Stop")) > stops })

	// While a turn is with the model: the model's answer waits until the
	// hall is submitting the notice that carries this request.
	var during int64
	var submittedInTurn bool
	c.model.during[c.model.count()+1] = func() {
		during = c.writerSends("ask", "synthetic request during the turn")
		for deadline := time.Now().Add(interactiveWait); !submittedInTurn && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			submittedInTurn = c.storeCount(`SELECT COUNT(*) FROM publish_attempt a JOIN publish_attempt_item i ON i.attempt_id = a.id
				WHERE i.message_id = ? AND a.submitting_at_ms IS NOT NULL`, during) == 1
		}
	}
	term.idle()
	stops = c.count(named("Stop"))
	term.keys("second synthetic prompt\r")
	term.await("the Stop of the typed turn", func() bool { return c.count(named("Stop")) > stops })
	if !submittedInTurn {
		t.Fatalf("the hall did not submit the notice for request %d while the turn was with the model", during)
	}
	term.await("the Stop of the notice's turn", func() bool { return c.count(named("Stop")) >= stops+2 })
	term.idle()

	all := c.events()
	session := all[0].Session
	got := turnShape(inSession(all, session))
	if session == "" || turnShape(all) != got {
		t.Fatalf("callbacks of more than one session, or of none: %q", turnShape(all))
	}
	// The first such line of the doctor report is the Claude Code adapter's.
	_, observed, _ := strings.Cut(c.owner("owner", "doctor"), "publish notices on")
	observed, _, _ = strings.Cut(observed, "\n")
	t.Logf("requests %d (idle) and %d (during a turn); callbacks: %s; %d model requests; publish notices%s", idle, during, got, c.model.count(), observed)
	want := idleTurns(4)
	if got != want || c.model.count() != 4 || !strings.Contains(observed, "2 submitted") || !strings.Contains(observed, "observed 2 own turn, 0 absorbed, 0 unknown") {
		t.Fatalf("publish notices: callbacks %q, %d model requests, notices%s; want %q, 4 requests and both notices observed as their own turn", got, c.model.count(), observed, want)
	}
	term.exit()
}

// interactiveTurnEndCheck is what the interactive turn-end run observed, as
// the evidence states it.
const interactiveTurnEndCheck = "an unread request stored during a turn continues it once with the pending line, " +
	"also when the turn's prompt was typed ahead; no evidence, a note or a read request does not"

// The turn-end rule as the adapter checks it in an interactive session, with
// the same cases as print mode. One more case covers a turn whose prompt
// was typed ahead: Claude Code reports that prompt under the running turn's
// ID, and the adapter still checks the turn it opens.
func TestClaudeCodeInteractiveTurnEndConformance(t *testing.T) {
	world := func(t *testing.T, tested bool) *conformance {
		t.Helper()
		c := newConformance(t)
		c.requireClaude()
		c.owner("owner", "start")
		c.owner("owner", "channel", "create", "checkout")
		c.owner("owner", "channel", "default", "checkout", "--repo", c.repo)
		c.owner("owner", "adapter", "set", "claude", "--enabled", "on")
		c.writer("join", "checkout", "--as", "writer")
		c.owner("owner", "budget", "grant", "checkout/@writer", "--roots", "1")
		c.installInteractive()
		if tested {
			c.importTurnEnd(c.version(), "interactive", "tested", "loopback run before the check: bootstraps the exact profile it measures")
		}
		return c
	}
	claims := func(c *conformance) int { return c.storeCount(`SELECT COUNT(*) FROM turn_end_notice`) }
	gated := func(t *testing.T) string {
		c := world(t, false)
		var id int64
		c.model.during[1] = func() { id = c.writerSends("ask", "synthetic request during the turn") }
		term := c.interactive()
		term.turn("first synthetic prompt")
		term.exit()
		if c.model.count() != 1 || c.carries(1, id) != 0 || claims(c) != 0 {
			t.Fatalf("without evidence: %d requests, %d claims", c.model.count(), claims(c))
		}
		return c.observedBuild("interactive")
	}
	continued := func(t *testing.T) string {
		c := world(t, true)
		var id int64
		c.model.during[1] = func() { id = c.writerSends("ask", "synthetic request during the turn") }
		term := c.interactive()
		term.submit("first synthetic prompt", func(e hookEvent) bool { return e.Name == "Stop" && e.StopActive }, "the Stop of the continued turn")
		term.exit()
		if c.model.count() != 2 {
			t.Fatalf("%d model requests, want the turn and one continuation", c.model.count())
		}
		if c.carries(1, id) != 0 || c.carries(2, id) != 1 {
			t.Fatalf("pending line in requests 1 and 2: %d, %d", c.carries(1, id), c.carries(2, id))
		}
		pid := `(SELECT principal_id FROM binding WHERE alias = 'claude1')`
		if n := claims(c); n != 1 {
			t.Errorf("%d turn claims, want 1", n)
		}
		if n := c.storeCount(`SELECT COUNT(*) FROM announcement WHERE via = 'turn_end' AND message_id = ? AND principal_id = `+pid, id); n != 1 {
			t.Errorf("request %d claimed %d times", id, n)
		}
		if n := c.storeCount(`SELECT COUNT(*) FROM recipient WHERE message_id = ? AND picked_up_at_ms IS NULL AND principal_id = `+pid, id); n != 1 {
			t.Error("the continuation picked the request up; it must stay unread in the inbox")
		}
		if n := c.committedClaims(); n != 1 {
			t.Errorf("doctor counts %d claims, want 1", n)
		}
		// The owner's import accepts the text the evidence file will carry.
		c.importTurnEnd(c.version(), "interactive", "tested", interactiveTurnEndCheck)
		return c.observedBuild("interactive")
	}
	note := func(t *testing.T) string {
		c := world(t, true)
		c.model.during[1] = func() { c.writerSends("note", "synthetic note during the turn") }
		term := c.interactive()
		term.turn("first synthetic prompt")
		term.exit()
		if c.model.count() != 1 || strings.Contains(c.model.request(1), stopContext) || claims(c) != 0 {
			t.Fatalf("a note continued the turn: %d requests", c.model.count())
		}
		return c.observedBuild("interactive")
	}
	alreadyRead := func(t *testing.T) string {
		c := world(t, true)
		var id int64
		c.model.during[1] = func() { id = c.writerSends("ask", "synthetic request read during the turn") }
		c.model.bash[1] = c.crosshall + " read"
		term := c.interactive("--allowedTools", "Bash")
		term.turn("first synthetic prompt")
		term.exit()
		pid := `(SELECT principal_id FROM binding WHERE alias = 'claude1')`
		if n := c.storeCount(`SELECT COUNT(*) FROM recipient WHERE message_id = ? AND picked_up_at_ms IS NOT NULL AND principal_id = `+pid, id); n != 1 {
			t.Fatalf("the session's read did not pick request %d up", id)
		}
		if c.model.count() != 2 || c.carries(2, id) != 0 || claims(c) != 0 {
			t.Fatalf("a request already read continued the turn: %d requests", c.model.count())
		}
		return c.observedBuild("interactive")
	}
	typedAhead := func(t *testing.T) string {
		c := world(t, true)
		term := c.interactive()
		c.model.during[1] = func() {
			prompts := c.count(named("UserPromptSubmit"))
			term.keys("second synthetic prompt\r")
			term.await("the typed-ahead prompt's callback", func() bool { return c.count(named("UserPromptSubmit")) > prompts })
		}
		var id int64
		c.model.during[2] = func() { id = c.writerSends("ask", "synthetic request during the typed-ahead turn") }
		term.keys("first synthetic prompt\r")
		term.await("the Stop of the continued typed-ahead turn", func() bool {
			return c.count(func(e hookEvent) bool { return e.Name == "Stop" && e.StopActive }) == 1
		})
		term.exit()
		// The continuation counts only for a turn that took the typed-ahead
		// shape: the second prompt under the first turn's ID, the first
		// turn's Stop, then the second turn's own Stop and its continued one.
		all := c.events()
		got := turnShape(inSession(all, all[0].Session))
		if want := typedAheadShape + ", ContinuedStop B"; all[0].Session == "" || got != want || turnShape(all) != got || id == 0 {
			t.Fatalf("not a continued typed-ahead turn with a stored request: callbacks %q, want %q, request %d", turnShape(all), want, id)
		}
		if c.model.count() != 3 {
			t.Fatalf("%d model requests, want both turns and one continuation", c.model.count())
		}
		if c.carries(1, id) != 0 || c.carries(2, id) != 0 || c.carries(3, id) != 1 {
			t.Fatalf("pending line in requests 1, 2 and 3: %d, %d, %d", c.carries(1, id), c.carries(2, id), c.carries(3, id))
		}
		pid := `(SELECT principal_id FROM binding WHERE alias = 'claude1')`
		if n := claims(c); n != 1 {
			t.Errorf("%d turn claims, want 1", n)
		}
		if n := c.storeCount(`SELECT COUNT(*) FROM announcement WHERE via = 'turn_end' AND message_id = ? AND principal_id = `+pid, id); n != 1 {
			t.Errorf("request %d claimed %d times", id, n)
		}
		if n := c.storeCount(`SELECT COUNT(*) FROM recipient WHERE message_id = ? AND picked_up_at_ms IS NULL AND principal_id = `+pid, id); n != 1 {
			t.Error("the continuation picked the request up; it must stay unread in the inbox")
		}
		return c.observedBuild("interactive")
	}
	run := &evidenceRun{t: t}
	run.run("no evidence", gated)
	run.run("tested", continued)
	run.run("note", note)
	run.run("already read", alreadyRead)
	run.run("typed ahead", typedAhead)

	// Every case ran to its end on one build: write the evidence row.
	version, write := run.evidenceBuild(os.Getenv("CROSSHALL_CONFORMANCE_OUT") != "")
	if !write {
		return
	}
	c := &conformance{t: t}
	c.writeEvidence("interactive-turn-end", version, "interactive", []evidenceResult{{"turn_end", "tested", interactiveTurnEndCheck}})
}
