//go:build conformance

package conformance

import (
	"bytes"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/ident"

	_ "modernc.org/sqlite"
)

// conformance is one disposable world: a temporary home, Crosshall state,
// a Claude Code configuration home and a loopback model endpoint.
type conformance struct {
	t                         *testing.T
	root, home, state, config string
	repo, crosshall, claude   string
	writerRoute               string
	model                     *fixture
	malformed                 sync.Once // reports a malformed callback record once
}

func newConformance(t *testing.T) *conformance {
	t.Helper()
	root := filepath.Join(t.TempDir(), "crosshall-conformance")
	c := &conformance{t: t, root: root, home: filepath.Join(root, "home"), state: filepath.Join(root, "state"),
		config: filepath.Join(root, "claude-config"), repo: filepath.Join(root, "home", "checkout")}
	for _, dir := range []string{c.home, filepath.Join(c.home, "tmp"), c.config, filepath.Join(c.repo, ".git")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Mkdir(c.state, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.repo, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c.crosshall = filepath.Join(root, "bin", "crosshall")
	build := exec.Command("go", "build", "-o", c.crosshall, "./cmd/crosshall")
	build.Dir = "../.."
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	var route [16]byte
	_, _ = rand.Read(route[:])
	c.writerRoute = hex.EncodeToString(route[:])
	c.model = newFixture(t)
	t.Cleanup(c.cleanup)
	return c
}

// requireClaude selects the real claude binary, or skips without one.
func (c *conformance) requireClaude() {
	c.t.Helper()
	claude, err := exec.LookPath("claude")
	if err != nil {
		c.t.Skip("no claude binary on PATH")
	}
	if c.claude, err = filepath.EvalSymlinks(claude); err != nil {
		c.t.Fatal(err)
	}
}

// claudeEnv is the scratch environment of every vendor subprocess: the
// temporary homes and the loopback endpoint, nothing ambient.
func (c *conformance) claudeEnv() []string {
	return append(c.base(), "CLAUDE_CONFIG_DIR="+c.config, "ANTHROPIC_BASE_URL="+c.model.server.URL,
		"ANTHROPIC_API_KEY=sk-ant-fixture-not-a-key", "DISABLE_TELEMETRY=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_AUTOUPDATER=1")
}

// safeOutputDir accepts only an absolute, existing directory with no linked
// component, outside the real home.
func safeOutputDir(dir string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("%s is not absolute", dir)
	}
	// The login account's home comes from the account database, not the
	// ambient HOME; without it the guard fails closed.
	account, err := user.Current()
	if err != nil || !filepath.IsAbs(account.HomeDir) {
		return fmt.Errorf("cannot resolve the login home to guard %s", dir)
	}
	return safeOutputDirFor(dir, account.HomeDir)
}

// safeOutputDirFor is safeOutputDir for a given login home.
func safeOutputDirFor(dir, login string) error {
	if !filepath.IsAbs(dir) {
		return fmt.Errorf("%s is not absolute", dir)
	}
	clean := filepath.Clean(dir)
	// Protected: the login home, a set HOME, and the vendor homes in each,
	// wherever their links lead. They are compared by file identity, so a
	// different spelling on a case-insensitive file system still matches.
	homes := []string{login}
	if env := os.Getenv("HOME"); filepath.IsAbs(env) {
		homes = append(homes, env)
	}
	var protected []os.FileInfo
	for _, home := range homes {
		for _, path := range []string{home, filepath.Join(home, ".codex"), filepath.Join(home, ".claude")} {
			info, err := os.Stat(path)
			if errors.Is(err, os.ErrNotExist) && path != login {
				continue
			}
			if err != nil {
				return fmt.Errorf("cannot inspect %s to guard %s", path, dir)
			}
			protected = append(protected, info)
		}
	}
	for ancestor := clean; ; ancestor = filepath.Dir(ancestor) {
		if info, err := os.Stat(ancestor); err == nil {
			for _, p := range protected {
				if os.SameFile(info, p) {
					return fmt.Errorf("%s is inside a home or vendor directory", dir)
				}
			}
		}
		if ancestor == filepath.Dir(ancestor) {
			break
		}
	}
	info, err := os.Lstat(clean)
	if err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a link", dir)
	}
	if err != nil || !info.IsDir() {
		return fmt.Errorf("%s is not an existing directory", dir)
	}
	if real, err := filepath.EvalSymlinks(clean); err != nil || real != clean {
		return fmt.Errorf("%s has a linked component", dir)
	}
	return nil
}

// base is the environment every child starts from; nothing is inherited.
func (c *conformance) base() []string {
	return []string{"HOME=" + c.home, "PATH=/usr/bin:/bin", "TMPDIR=" + filepath.Join(c.home, "tmp"),
		"CROSSHALL_STATE_DIR=" + c.state}
}

func (c *conformance) crosshallRun(extra []string, args ...string) (string, error) {
	cmd := exec.Command(c.crosshall, args...)
	cmd.Env, cmd.Dir = append(c.base(), extra...), c.home
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (c *conformance) owner(args ...string) string {
	c.t.Helper()
	out, err := c.crosshallRun(nil, args...)
	if err != nil {
		c.t.Fatalf("crosshall %v: %v\n%s", args, err, out)
	}
	return out
}

func (c *conformance) writer(args ...string) string {
	c.t.Helper()
	out, err := c.crosshallRun([]string{"CROSSHALL_ROUTE=" + c.writerRoute}, args...)
	if err != nil {
		c.t.Fatalf("writer %v: %v\n%s", args, err, out)
	}
	return out
}

// install writes the manifest's install description into the temporary
// configuration home, the way setup does, plus a probe entry that records
// the hook environment's launch-bound version and mode.
func (c *conformance) install() {
	c.t.Helper()
	raw, err := os.ReadFile("../../internal/hook/manifests/claude.json")
	if err != nil {
		c.t.Fatal(err)
	}
	var m struct {
		Install struct {
			Events   []string          `json:"events"`
			Matchers map[string]string `json:"matchers"`
			Timeout  int               `json:"timeout_s"`
		} `json:"install"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		c.t.Fatal(err)
	}
	hooks := map[string]any{}
	for _, event := range m.Install.Events {
		group := map[string]any{"hooks": []any{map[string]any{
			"type": "command", "command": c.crosshall, "args": []string{"hook", "claude", event}, "timeout": m.Install.Timeout}}}
		if matcher := m.Install.Matchers[event]; matcher != "" {
			group["matcher"] = matcher
		}
		hooks[event] = []any{group}
	}
	probe := filepath.Join(c.root, "probe.sh")
	script := "#!/bin/sh\ncat >/dev/null\nprintf 'AI_AGENT=%s\\nENTRYPOINT=%s\\n' \"$AI_AGENT\" \"$CLAUDE_CODE_ENTRYPOINT\" > " +
		filepath.Join(c.root, "probe.env") + "\n"
	if err := os.WriteFile(probe, []byte(script), 0700); err != nil {
		c.t.Fatal(err)
	}
	hooks["SessionStart"] = append(hooks["SessionStart"].([]any),
		map[string]any{"hooks": []any{map[string]any{"type": "command", "command": probe, "timeout": 5}}})
	settings, _ := json.MarshalIndent(map[string]any{"hooks": hooks}, "", "  ")
	if err := os.WriteFile(filepath.Join(c.config, "settings.json"), settings, 0600); err != nil {
		c.t.Fatal(err)
	}
}

// claudeRun runs `claude -p` in the temporary homes against the loopback endpoint.
func (c *conformance) claudeRun(stdin string, args ...string) string {
	c.t.Helper()
	cmd := exec.Command(c.claude, append([]string{"-p", "--model", "haiku"}, args...)...)
	cmd.Dir, cmd.Stdin = c.repo, strings.NewReader(stdin)
	cmd.Env = c.claudeEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		c.t.Fatalf("claude %v: %v\n%s", args, err, stderr.String())
	}
	return stdout.String()
}

// claudeTurns runs one `claude -p` session with stream-json input, sending
// each prompt only after the previous turn's result, so every prompt is its
// own turn. It returns the session's stream-json output.
func (c *conformance) claudeTurns(prompts ...string) string {
	c.t.Helper()
	cmd := exec.Command(c.claude, "-p", "--model", "haiku", "--input-format", "stream-json",
		"--output-format", "stream-json", "--verbose")
	cmd.Dir = c.repo
	cmd.Env = c.claudeEnv()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		c.t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		c.t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		c.t.Fatal(err)
	}
	send := func(text string) {
		line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}})
		_, _ = stdin.Write(append(line, '\n'))
	}
	var out strings.Builder
	send(prompts[0])
	next := 1
	decoder := json.NewDecoder(stdout)
	for {
		var msg map[string]any
		if err := decoder.Decode(&msg); err != nil {
			break
		}
		raw, _ := json.Marshal(msg)
		out.Write(append(raw, '\n'))
		if msg["type"] == "result" {
			if next == len(prompts) {
				_ = stdin.Close()
				continue
			}
			send(prompts[next])
			next++
		}
	}
	if err := cmd.Wait(); err != nil {
		c.t.Fatalf("claude session: %v\n%s", err, stderr.String())
	}
	return out.String()
}

type launchProfile struct {
	VendorVersion string `json:"vendor_version"`
	Mode          string `json:"mode"`
}

// launchProfiles reads the recorded launch profiles from the temporary store.
func (c *conformance) launchProfiles() []launchProfile {
	c.t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(c.state, "store.sqlite3")+"?mode=ro")
	if err != nil {
		c.t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT profile FROM adapter_launch`)
	if err != nil {
		c.t.Fatal(err)
	}
	defer rows.Close()
	var out []launchProfile
	for rows.Next() {
		var raw string
		var p launchProfile
		if rows.Scan(&raw) == nil && json.Unmarshal([]byte(raw), &p) == nil {
			out = append(out, p)
		}
	}
	return out
}

func (c *conformance) version() string {
	probe := exec.Command(c.claude, "--version")
	probe.Env, probe.Dir = c.claudeEnv(), c.repo
	out, err := probe.Output()
	if err != nil {
		c.t.Fatal(err)
	}
	return strings.Fields(string(out))[0]
}

func (c *conformance) cleanup() {
	// Stop the disposable dispatcher this run started, then remove its roots.
	rendezvous := ident.Rendezvous(c.state, os.Geteuid())
	if raw, err := os.ReadFile(filepath.Join(rendezvous, "ready")); err == nil {
		var marker struct {
			PID int `json:"pid"`
		}
		if json.Unmarshal(raw, &marker) == nil && marker.PID > 0 {
			_ = syscall.Kill(marker.PID, syscall.SIGTERM)
			time.Sleep(300 * time.Millisecond)
		}
	}
	c.model.server.Close()
	_ = os.RemoveAll(rendezvous)
	// Claude Code keeps a per-directory scratch folder under /tmp/claude-<uid>,
	// named after the working directory's real path.
	if real, err := filepath.EvalSymlinks(c.repo); err == nil {
		slug := regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(real, "-")
		if strings.Contains(slug, "crosshall-conformance") {
			_ = os.RemoveAll(filepath.Join("/tmp", fmt.Sprintf("claude-%d", os.Getuid()), slug))
		}
	}
}

// fixture is the loopback stand-in for the model endpoint. It records every
// message request and can run an action while a given request is in flight.
type fixture struct {
	server   *httptest.Server
	mu       sync.Mutex
	requests []string
	during   map[int]func()
	// bash answers request n with a Bash tool call running this command.
	bash map[int]string
}

func newFixture(t *testing.T) *fixture {
	f := &fixture{during: map[int]func(){}, bash: map[int]string{}}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/v1/messages") || strings.Contains(r.URL.Path, "count_tokens") {
			w.Header().Set("content-type", "application/json")
			_, _ = w.Write([]byte(`{"input_tokens":1}`))
			return
		}
		f.mu.Lock()
		f.requests = append(f.requests, string(body))
		n := len(f.requests)
		action, command := f.during[n], f.bash[n]
		f.mu.Unlock()
		if action != nil {
			action()
		}
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(body, &req)
		w.Header().Set("content-type", "text/event-stream")
		usage := map[string]int{"input_tokens": 1, "output_tokens": 1}
		block := []struct {
			name string
			data any
		}{
			{"content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}}},
			{"content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "OK"}}},
		}
		stop := "end_turn"
		if command != "" {
			input, _ := json.Marshal(map[string]string{"command": command})
			block = []struct {
				name string
				data any
			}{
				{"content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{
					"type": "tool_use", "id": fmt.Sprintf("toolu_fixture%02d", n), "name": "Bash", "input": map[string]any{}}}},
				{"content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{
					"type": "input_json_delta", "partial_json": string(input)}}},
			}
			stop = "tool_use"
		}
		events := []struct {
			name string
			data any
		}{
			{"message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_fixture", "type": "message",
				"role": "assistant", "model": req.Model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil, "usage": usage}}},
		}
		events = append(events, block...)
		events = append(events, []struct {
			name string
			data any
		}{
			{"content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}},
			{"message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 1}}},
			{"message_stop", map[string]any{"type": "message_stop"}},
		}...)
		for _, event := range events {
			data, _ := json.Marshal(event.data)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event.name, data)
		}
	}))
	return f
}

func (f *fixture) request(n int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n < 1 || n > len(f.requests) {
		return ""
	}
	return f.requests[n-1]
}

func (f *fixture) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

type evidenceResult struct {
	Cap   string `json:"cap"`
	Level string `json:"level"`
	Check string `json:"check"`
}

func TestClaudeCodePrintModeConformance(t *testing.T) {
	c := newConformance(t)
	c.requireClaude()
	c.owner("owner", "start")
	c.owner("owner", "channel", "create", "checkout")
	c.owner("owner", "channel", "default", "checkout", "--repo", c.repo)
	c.owner("owner", "adapter", "set", "claude", "--enabled", "on")
	c.writer("join", "checkout", "--as", "writer")
	c.install()
	version := c.version()

	// One session, three prompts (stream-json input). Mail arrives while the
	// first turn is with the model, so it is due at the second prompt; the
	// third prompt is a quiet turn.
	var noteID, observedFact string
	c.model.during[1] = func() {
		out := c.writer("--json", "note", "@claude1", "synthetic conformance note")
		var r struct {
			ID int64 `json:"id"`
		}
		_ = json.Unmarshal([]byte(out), &r)
		noteID = fmt.Sprint(r.ID)
	}
	c.model.during[3] = func() {
		out := c.writer("--json", "who")
		var r struct {
			Members []struct {
				Alias    string `json:"alias"`
				Observed *struct {
					Fact string `json:"fact"`
				} `json:"observed"`
			} `json:"members"`
		}
		_ = json.Unmarshal([]byte(out), &r)
		for _, m := range r.Members {
			if m.Alias == "claude1" && m.Observed != nil {
				observedFact = m.Observed.Fact
			}
		}
	}
	stream := c.claudeTurns("first synthetic prompt", "second synthetic prompt", "third synthetic prompt")
	session := regexp.MustCompile(`"session_id":"([0-9a-f-]{36})"`).FindStringSubmatch(stream)
	if session == nil || c.model.count() != 3 || noteID == "" {
		t.Fatalf("session %v, %d model requests, note %q", session, c.model.count(), noteID)
	}
	logRequestTokenSamples(t, "claude", []byte(c.model.request(1)), "hint")
	logRequestTokenSamples(t, "claude", []byte(c.model.request(2)), "pending")
	hint := "Joined #checkout as @claude1"
	pending := "[crosshall] pending: " + noteID
	enrolled := strings.Contains(c.model.request(1), hint)
	offered := !strings.Contains(c.model.request(1), pending) && strings.Count(c.model.request(2), pending) == 1 &&
		strings.Count(c.model.request(3), pending) == 1
	quiet := strings.Count(c.model.request(3), "[crosshall]") == strings.Count(c.model.request(2), "[crosshall]")
	departed := !strings.Contains(c.writer("who"), "@claude1")

	// The next start of the same session restores the member.
	restored := false
	c.model.during[4] = func() { restored = strings.Contains(c.writer("who"), "@claude1") }
	c.claudeRun("", "--resume", session[1], "fourth synthetic prompt")

	probe, _ := os.ReadFile(filepath.Join(c.root, "probe.env"))
	t.Logf("claude %s; hook environment: %s", version, strings.ReplaceAll(strings.TrimSpace(string(probe)), "\n", ", "))
	launchVersion := strings.Contains(string(probe), "claude-code_"+strings.ReplaceAll(version, ".", "-"))
	// The hook carried that build and mode into the launch profile.
	profiles := c.launchProfiles()
	launchProfile := len(profiles) > 0
	for _, p := range profiles {
		launchProfile = launchProfile && p.VendorVersion == version && p.Mode == "print"
	}

	provenance := launchVersion && launchProfile
	level := func(ok bool) string {
		if ok && provenance {
			return "tested"
		}
		return "candidate"
	}
	results := []evidenceResult{
		{"enroll", level(enrolled && departed && restored), "auto-join hint at start; clean end departs; next start restores"},
		{"boundary", level(offered && quiet), "pending line once at the prompt after the mail; quiet turn adds nothing"},
		{"observe", level(observedFact == "prompt_submitted" && departed), "prompt fact visible to another member; end departs"},
		reminderEvidence(readProcessCallID(t)),
	}
	for name, ok := range map[string]bool{"enrolled": enrolled, "offered": offered, "quiet": quiet,
		"departed": departed, "restored": restored, "launch version": launchVersion, "launch profile": launchProfile} {
		if !ok {
			t.Errorf("%s: not observed", name)
		}
	}
	if observedFact != "prompt_submitted" {
		t.Errorf("observed fact %q", observedFact)
	}

	if t.Failed() {
		t.Fatal("a check failed: no evidence is written or recorded")
	}
	evidence, _ := json.MarshalIndent(map[string]any{"vendor": "claude", "vendor_version": version,
		"platform": runtime.GOOS + "-" + runtime.GOARCH, "mode": "print", "manifest_rev": "claude-2",
		"run_at_ms": time.Now().UnixMilli(), "results": results}, "", "  ")
	dir := os.Getenv("CROSSHALL_CONFORMANCE_OUT")
	if dir == "" {
		dir = c.root
	} else if err := safeOutputDir(dir); err != nil {
		t.Fatalf("CROSSHALL_CONFORMANCE_OUT: %v", err)
	}
	file := filepath.Join(dir, fmt.Sprintf("claude-%s-%s-%s-print.json", version, runtime.GOOS, runtime.GOARCH))
	out, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := out.Write(append(evidence, '\n')); err != nil || out.Close() != nil {
		t.Fatal("cannot write the evidence file")
	}
	t.Logf("evidence: %s\n%s", file, evidence)
	if out := c.owner("owner", "adapter", "evidence", file); !strings.Contains(out, "Recorded 4 result(s)") {
		t.Fatalf("evidence import: %s", out)
	}
	// The imported rows are the exported levels for this exact profile.
	recorded := c.evidenceLevels(version, runtime.GOOS+"-"+runtime.GOARCH, "print", "claude-2")
	for _, r := range results {
		if recorded[r.Cap] != r.Level {
			t.Errorf("imported %s %q, exported %q", r.Cap, recorded[r.Cap], r.Level)
		}
	}
}

// reminderEvidence is the reminder result of a run, from callIDVar, the
// read-process variable that carries the native tool-call ID ("" for none).
// Without one the dispatcher cannot establish which tool call a read ran in,
// so the reminder is unavailable (issue #20). With one it is a candidate: the
// owner's trigger to reopen the parked read-origin path.
func reminderEvidence(callIDVar string) evidenceResult {
	if callIDVar == "" {
		return evidenceResult{"reminder", "unavailable",
			"no native tool-call ID reaches the read process, so a read's origin cannot be established"}
	}
	return evidenceResult{"reminder", "candidate",
		"the read process carries the native tool-call ID in " + callIDVar + "; the read-origin path is parked"}
}

// readProcessCallID runs one "crosshall read" Bash call under real Claude
// Code and returns the name of a variable of that process whose value holds
// the call's tool_use_id, as the post-tool hook reports it, or "".
func readProcessCallID(t *testing.T) string {
	t.Helper()
	c := newConformance(t)
	c.requireClaude()
	fakeBin := filepath.Join(c.root, "fakebin")
	if err := os.Mkdir(fakeBin, 0700); err != nil {
		t.Fatal(err)
	}
	readEnv, post := filepath.Join(c.root, "read.env"), filepath.Join(c.root, "post.json")
	if err := os.WriteFile(filepath.Join(fakeBin, "crosshall"), []byte("#!/bin/sh\nset -e\nenv -0 > "+readEnv+".part\nmv "+readEnv+".part "+readEnv+"\necho '#41 ask from @writer'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	logger := filepath.Join(c.root, "post.sh")
	if err := os.WriteFile(logger, []byte("#!/bin/sh\ncat > "+post+"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	settings, _ := json.Marshal(map[string]any{"hooks": map[string]any{"PostToolUse": []any{
		map[string]any{"matcher": "Bash", "hooks": []any{map[string]any{"type": "command", "command": logger, "timeout": 5}}}}}})
	if err := os.WriteFile(filepath.Join(c.config, "settings.json"), settings, 0600); err != nil {
		t.Fatal(err)
	}
	c.model.bash[1] = "crosshall read"
	env := c.claudeEnv()
	for i, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			env[i] = "PATH=" + fakeBin + ":/usr/bin:/bin"
		}
	}
	cmd := exec.Command(c.claude, "-p", "--model", "haiku", "--allowedTools", "Bash")
	cmd.Dir, cmd.Env, cmd.Stdin = c.repo, env, strings.NewReader("probe")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("claude: %v\n%s", err, stderr.String())
	}
	var payload struct {
		ToolUseID string `json:"tool_use_id"`
	}
	// The capture is published only after the environment command succeeded.
	vars, err := os.ReadFile(readEnv)
	if err != nil {
		t.Fatalf("read-process environment capture: %v", err)
	}
	raw, _ := os.ReadFile(post)
	if json.Unmarshal(raw, &payload) != nil || payload.ToolUseID == "" {
		t.Fatalf("the probe read did not run under a post-tool hook: %q", raw)
	}
	name, err := callIDVariable(vars, payload.ToolUseID)
	if err != nil {
		t.Fatalf("read-process environment capture: %v", err)
	}
	return name
}

// callIDVariable returns the name of the entry of environ ("env -0" output)
// whose value holds id, or "". A capture that is empty, not NUL-terminated,
// has an entry without a name, or lacks the PATH the fixture set is an error,
// never "no call ID".
func callIDVariable(environ []byte, id string) (string, error) {
	text, complete := strings.CutSuffix(string(environ), "\x00")
	if !complete || text == "" {
		return "", errors.New("empty or truncated")
	}
	var found string
	var path bool
	for _, entry := range strings.Split(text, "\x00") {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" {
			return "", fmt.Errorf("malformed entry %q", entry)
		}
		path = path || name == "PATH"
		if found == "" && strings.Contains(value, id) {
			found = name
		}
	}
	if !path {
		return "", errors.New("no PATH entry")
	}
	return found, nil
}

// evidenceLevels reads the recorded capability levels of one profile from the
// temporary store.
func (c *conformance) evidenceLevels(version, platform, mode, rev string) map[string]string {
	c.t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(c.state, "store.sqlite3")+"?mode=ro")
	if err != nil {
		c.t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT cap, level FROM capability_evidence WHERE vendor = 'claude'
		AND vendor_version = ? AND platform = ? AND mode = ? AND manifest_rev = ?`, version, platform, mode, rev)
	if err != nil {
		c.t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var cap, level string
		if err := rows.Scan(&cap, &level); err != nil {
			c.t.Fatal(err)
		}
		out[cap] = level
	}
	return out
}

// The install description's one-second timeout is honoured, so a
// stalled hook cannot hold a session start for longer.
func TestClaudeCodeHonoursTheOneSecondHookTimeout(t *testing.T) {
	c := newConformance(t)
	c.requireClaude()
	slow := filepath.Join(c.root, "slow.sh")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\ncat >/dev/null\nsleep 5\n"), 0700); err != nil {
		t.Fatal(err)
	}
	settings, _ := json.Marshal(map[string]any{"hooks": map[string]any{"SessionStart": []any{
		map[string]any{"hooks": []any{map[string]any{"type": "command", "command": slow, "timeout": 1}}}}}})
	if err := os.WriteFile(filepath.Join(c.config, "settings.json"), settings, 0600); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	c.claudeRun("", "synthetic prompt")
	if took := time.Since(began); took > 4*time.Second || c.model.count() != 1 {
		t.Fatalf("a 5 s hook with timeout 1 held the session %s (%d model requests)", took, c.model.count())
	} else {
		t.Logf("session with a stalled start hook finished in %s", took.Round(10*time.Millisecond))
	}
}

// Every vendor subprocess, the version probe included, runs in the scratch
// environment and directory, never the ambient ones.
func TestTheVersionProbeUsesTheScratchEnvironment(t *testing.T) {
	c := newConformance(t)
	fake := filepath.Join(c.root, "fake-claude")
	record := filepath.Join(c.root, "fake-claude.env")
	script := "#!/bin/sh\nprintf 'HOME=%s\\nCONFIG=%s\\nCWD=%s\\n' \"$HOME\" \"$CLAUDE_CONFIG_DIR\" \"$(pwd -P)\" > " + record +
		"\necho '9.9.9 (Claude Code)'\n"
	if err := os.WriteFile(fake, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	c.claude = fake
	if got := c.version(); got != "9.9.9" {
		t.Fatalf("version %q", got)
	}
	raw, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	repo, _ := filepath.EvalSymlinks(c.repo)
	for _, want := range []string{"HOME=" + c.home + "\n", "CONFIG=" + c.config + "\n", "CWD=" + repo + "\n"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the version probe saw %q, missing %q", raw, want)
		}
	}
}

// The evidence override must be a real, unlinked directory outside the home.
func TestTheEvidenceOutputMustBeASafeDirectory(t *testing.T) {
	good := t.TempDir()
	if real, err := filepath.EvalSymlinks(good); err == nil {
		good = real
	}
	if err := safeOutputDir(good); err != nil {
		t.Fatalf("a plain temporary directory was refused: %v", err)
	}
	link := filepath.Join(good, "link")
	if err := os.Symlink(good, link); err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	for _, bad := range []string{"relative/dir", link, filepath.Join(good, "missing"), filepath.Join(home, ".claude")} {
		if err := safeOutputDir(bad); err == nil {
			t.Errorf("%s was accepted", bad)
		}
	}
}

// The home guard resolves the login account's home itself, so a redirected
// or missing HOME cannot let the real home through.
func TestTheEvidenceOutputGuardIgnoresTheAmbientHome(t *testing.T) {
	account, err := user.Current()
	if err != nil || !filepath.IsAbs(account.HomeDir) {
		t.Skip("no login home to check against")
	}
	login := account.HomeDir
	if real, err := filepath.EvalSymlinks(login); err == nil {
		login = real
	}
	for name, set := range map[string]func(){
		"redirected": func() { t.Setenv("HOME", t.TempDir()) },
		"unset":      func() { t.Setenv("HOME", ""); os.Unsetenv("HOME") },
	} {
		set()
		if err := safeOutputDir(login); err == nil {
			t.Errorf("HOME %s: the login home was accepted", name)
		}
	}
}

// A vendor home linked to another volume, and the home spelled in another
// case on a case-insensitive file system, are refused as well.
func TestTheEvidenceOutputGuardFollowsVendorLinksAndFileIdentity(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	login, elsewhere := filepath.Join(root, "login"), filepath.Join(root, "elsewhere")
	for _, dir := range []string{login, filepath.Join(elsewhere, "codex", "sessions"), filepath.Join(elsewhere, "claude")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for name, target := range map[string]string{".codex": "codex", ".claude": "claude"} {
		if err := os.Symlink(filepath.Join(elsewhere, target), filepath.Join(login, name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", login)
	for _, bad := range []string{filepath.Join(elsewhere, "codex"), filepath.Join(elsewhere, "codex", "sessions"), filepath.Join(elsewhere, "claude")} {
		if err := safeOutputDirFor(bad, login); err == nil {
			t.Errorf("%s, a linked vendor home, was accepted", bad)
		}
	}
	if err := safeOutputDirFor(elsewhere, login); err != nil {
		t.Errorf("a directory that only contains the vendor targets was refused: %v", err)
	}
	// Another spelling of the same directory, where the file system allows it.
	upper := filepath.Join(root, "LOGIN")
	if info, err := os.Stat(upper); err == nil && info.IsDir() {
		if err := safeOutputDirFor(upper, login); err == nil {
			t.Error("the login home in another case was accepted")
		}
	}
}

// TestClaudeCodePostToolUseProbe pins, for the real binary in print mode,
// what the post-tool hook receives, which Bash commands the parked
// condition lets through, and how each Stop output form reaches the model.
// A fake crosshall on PATH prints a receipt line.
func TestClaudeCodePostToolUseProbe(t *testing.T) {
	t.Run("context", func(t *testing.T) {
		copies := postToolUseProbe(t, `{"hookSpecificOutput":{"hookEventName":"Stop","additionalContext":"[probe] stop context"}}`)
		// One copy, in a system-role conversation message, naming nothing local.
		if len(copies) != 1 || copies[0].role != "system" || !strings.Contains(copies[0].text, "Stop hook additional context: [probe] stop context") {
			t.Fatalf("additionalContext reached the model as %+v", copies)
		}
	})
	t.Run("block", func(t *testing.T) {
		copies := postToolUseProbe(t, `{"decision":"block","reason":"[probe] stop context"}`)
		// Two copies: user-role feedback, and a system-role copy that names the
		// hook command's absolute path - why the adapter uses additionalContext.
		if len(copies) != 2 || copies[0].role != "user" || copies[1].role != "system" ||
			!strings.Contains(copies[1].text, "stop.sh") {
			t.Fatalf("a block reason reached the model as %+v", copies)
		}
	})
}

type stopCopy struct{ role, text string }

// parkedReadCondition is the post-tool "if" condition of the parked reply
// reminder (issue #20): it is measured here and shipped in no manifest.
const parkedReadCondition = "Bash(crosshall read *)"

func postToolUseProbe(t *testing.T, stopOutput string) []stopCopy {
	c := newConformance(t)
	c.requireClaude()
	fakeBin := filepath.Join(c.root, "fakebin")
	if err := os.Mkdir(fakeBin, 0700); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(fakeBin, "crosshall")
	receipt := "[crosshall] receipt: rfixturefixturefixturefixtur"
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho '#41 ask from @writer'\necho '"+receipt+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(c.root, "hooklog")
	if err := os.Mkdir(logDir, 0700); err != nil {
		t.Fatal(err)
	}
	logger := func(name, extra string) string {
		path := filepath.Join(c.root, name+".sh")
		script := "#!/bin/sh\nf=$(mktemp " + filepath.Join(logDir, name+".XXXXXX") + ")\ncat > \"$f\"\n" + extra
		if err := os.WriteFile(path, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	marker := filepath.Join(c.root, "stop-once")
	stopExtra := "if [ ! -e " + marker + " ]; then : > " + marker + "; echo '" + stopOutput + "'; fi\n"
	settings, _ := json.Marshal(map[string]any{"hooks": map[string]any{
		"PostToolUse": []any{
			map[string]any{"matcher": "Bash", "hooks": []any{
				map[string]any{"type": "command", "command": logger("post-all", ""), "timeout": 5}}},
			map[string]any{"matcher": "Bash", "hooks": []any{
				map[string]any{"type": "command", "command": logger("post-read", ""), "timeout": 5,
					"if": parkedReadCondition}}},
		},
		"Stop": []any{map[string]any{"hooks": []any{
			map[string]any{"type": "command", "command": logger("stop", stopExtra), "timeout": 5}}}},
	}})
	if err := os.WriteFile(filepath.Join(c.config, "settings.json"), settings, 0600); err != nil {
		t.Fatal(err)
	}
	commands := []string{"crosshall read", "echo unrelated", fake + " read --json", "crosshall read 41 > /dev/null",
		"crosshall read --json 41", "crosshall reader", "crosshall --json read 41", "crosshall --key k read 41",
		"crosshall read 41 | cat", "crosshall read 41; echo x", "echo x; crosshall read 41", "crosshall readx 41"}
	for i, command := range commands {
		c.model.bash[i+1] = command
	}
	env := c.claudeEnv()
	for i, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			env[i] = "PATH=" + fakeBin + ":/usr/bin:/bin"
		}
	}
	cmd := exec.Command(c.claude, "-p", "--model", "haiku", "--allowedTools", "Bash")
	cmd.Dir, cmd.Env, cmd.Stdin = c.repo, env, strings.NewReader("probe")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("claude: %v\n%s", err, stderr.String())
	}

	type payload struct {
		ToolUseID    string            `json:"tool_use_id"`
		PromptID     string            `json:"prompt_id"`
		StopActive   *bool             `json:"stop_hook_active"`
		ToolInput    map[string]string `json:"tool_input"`
		ToolResponse struct {
			Stdout string `json:"stdout"`
		} `json:"tool_response"`
	}
	logs := map[string][]payload{}
	entries, _ := os.ReadDir(logDir)
	for _, e := range entries {
		body, _ := os.ReadFile(filepath.Join(logDir, e.Name()))
		var p payload
		if err := json.Unmarshal(body, &p); err != nil {
			t.Fatal(err)
		}
		name := strings.SplitN(e.Name(), ".", 2)[0]
		logs[name] = append(logs[name], p)
	}
	// The condition starts the hook for every command line with a
	// "crosshall read" word sequence on PATH, compound ones included, and
	// for nothing else: not a longer word, an absolute path or a global
	// option before "read". Only the plain form could bind a receipt, so
	// an extractor would have to refuse the compound ones it lets through.
	var read []string
	for _, p := range logs["post-read"] {
		command := p.ToolInput["command"]
		read = append(read, command)
		if !strings.HasPrefix(p.ToolUseID, "toolu_fixture") || p.PromptID == "" {
			t.Errorf("post-tool payload without its tool call or turn: %+v", p)
		}
		quiet := command == "crosshall read 41 > /dev/null" || command == "crosshall read 41; echo x"
		if !quiet && !strings.HasSuffix(p.ToolResponse.Stdout, receipt) {
			t.Errorf("tool_response.stdout does not end with the receipt: %q", p.ToolResponse.Stdout)
		}
	}
	sort.Strings(read)
	if want := []string{"crosshall read", "crosshall read --json 41", "crosshall read 41 > /dev/null",
		"crosshall read 41 | cat", "crosshall read 41; echo x", "echo x; crosshall read 41"}; !slices.Equal(read, want) ||
		len(logs["post-all"]) != len(commands) {
		t.Fatalf("condition passed %q of %d post-tool callbacks", read, len(logs["post-all"]))
	}
	// The continued Stop says so.
	if s := logs["stop"]; len(s) != 2 || s[0].StopActive == nil || s[1].StopActive == nil || *s[0].StopActive == *s[1].StopActive {
		t.Fatalf("stop callbacks %+v", s)
	}
	// The Stop output costs exactly one more model request.
	if n := c.model.count(); n != len(commands)+2 {
		t.Fatalf("%d model requests", n)
	}
	var req struct {
		System   json.RawMessage `json:"system"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(c.model.request(c.model.count())), &req); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(req.System), "[probe] stop context") {
		t.Fatal("the Stop output reached the system prompt")
	}
	var copies []stopCopy
	for _, msg := range req.Messages {
		text := string(msg.Content)
		if at := strings.Index(text, "[probe] stop context"); at >= 0 {
			copies = append(copies, stopCopy{msg.Role, text[max(0, at-160) : at+len("[probe] stop context")]})
		}
	}
	t.Logf("stop output %s: %d model requests; copies %+v", stopOutput, c.model.count(), copies)
	return copies
}

// TestClaudeCodeStopContextRole records the role of the conversation message
// that carries a Stop hook's additional context, by how many tool calls the
// turn made and whether input was plain or stream-json.
func TestClaudeCodeStopContextRole(t *testing.T) {
	for _, tools := range []int{0, 1, 5} {
		for _, stream := range []bool{false, true} {
			t.Run(fmt.Sprintf("tools=%d stream=%v", tools, stream), func(t *testing.T) {
				c := newConformance(t)
				c.requireClaude()
				marker := filepath.Join(c.root, "stop-once")
				stop := filepath.Join(c.root, "stop.sh")
				script := "#!/bin/sh\ncat >/dev/null\nif [ ! -e " + marker + " ]; then : > " + marker +
					"; echo '{\"hookSpecificOutput\":{\"hookEventName\":\"Stop\",\"additionalContext\":\"[probe] stop context\"}}'; fi\n"
				if err := os.WriteFile(stop, []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				settings, _ := json.Marshal(map[string]any{"hooks": map[string]any{"Stop": []any{map[string]any{"hooks": []any{
					map[string]any{"type": "command", "command": stop, "timeout": 5}}}}}})
				if err := os.WriteFile(filepath.Join(c.config, "settings.json"), settings, 0600); err != nil {
					t.Fatal(err)
				}
				for i := 1; i <= tools; i++ {
					c.model.bash[i] = fmt.Sprintf("echo step%d", i)
				}
				args := []string{"-p", "--model", "haiku", "--allowedTools", "Bash"}
				stdin := "probe"
				if stream {
					args = append(args, "--input-format", "stream-json", "--output-format", "stream-json", "--verbose")
					stdin = `{"type":"user","message":{"role":"user","content":"probe"}}` + "\n"
				}
				cmd := exec.Command(c.claude, args...)
				cmd.Dir, cmd.Env, cmd.Stdin = c.repo, c.claudeEnv(), strings.NewReader(stdin)
				var stderr bytes.Buffer
				cmd.Stderr = &stderr
				if err := cmd.Run(); err != nil {
					t.Fatalf("claude: %v\n%s", err, stderr.String())
				}
				var req struct {
					Messages []struct {
						Role    string          `json:"role"`
						Content json.RawMessage `json:"content"`
					} `json:"messages"`
				}
				last := c.model.request(c.model.count())
				if err := json.Unmarshal([]byte(last), &req); err != nil {
					t.Fatal(err)
				}
				// One extra request; one copy; its role depends on the turn: a
				// quiet turn wraps it as a user-role system reminder, a turn
				// with tool calls appends it to a trailing system-role message.
				var carriers []string
				for _, msg := range req.Messages {
					if text := string(msg.Content); strings.Contains(text, "Stop hook additional context: [probe] stop context") {
						role := msg.Role
						if strings.Contains(text, "<system-reminder>") {
							role += "+system-reminder"
						}
						carriers = append(carriers, role)
					}
				}
				want := "system"
				if tools == 0 {
					want = "user+system-reminder"
				}
				if c.model.count() != tools+2 || len(carriers) != 1 || carriers[0] != want {
					t.Fatalf("%d requests; carriers %v, want one %s", c.model.count(), carriers, want)
				}
			})
		}
	}
}

// The reminder row follows this run's own measurement, so a vendor build
// that hands the read process its tool-call ID shows up as a changed row.
func TestReminderEvidenceFollowsTheCallIDMeasurement(t *testing.T) {
	if got := reminderEvidence(""); got.Cap != "reminder" || got.Level != "unavailable" || got.Check == "" {
		t.Errorf("no call ID: %+v", got)
	}
	if got := reminderEvidence("VENDOR_TOOL_USE_ID"); got.Level != "candidate" || !strings.Contains(got.Check, "VENDOR_TOOL_USE_ID") {
		t.Errorf("call ID present: %+v", got)
	}
}

// The probe keeps each environment entry whole, so an ID inside a multiline
// value is still attributed to its variable.
func TestTheCallIDProbeKeepsEnvironmentEntriesWhole(t *testing.T) {
	const id = "toolu_fixture01"
	environ := func(entries ...string) []byte { return []byte(strings.Join(entries, "\x00") + "\x00") }
	for _, tc := range []struct {
		name, want string
		environ    []byte
	}{
		{"absent", "", environ("HOME=/fixture", "PATH=/usr/bin:/bin")},
		{"single line", "ANY_NAME", environ("PATH=/usr/bin", "ANY_NAME=prefix-"+id+"-suffix")},
		{"multiline value", "TOOL_CONTEXT", environ("PATH=/usr/bin", "TOOL_CONTEXT={\n  \"call\": \""+id+"\"\n}")},
		{"name only", "", environ("PATH=/usr/bin", id+"=value")},
	} {
		if got, err := callIDVariable(tc.environ, id); got != tc.want || err != nil {
			t.Errorf("%s: got %q, %v, want %q", tc.name, got, err, tc.want)
		}
	}
	// A failed or partial capture is no measurement: it must not read as
	// "no call ID".
	for name, capture := range map[string][]byte{
		"empty":      nil,
		"truncated":  []byte("PATH=/usr/bin\x00HOME=/fix"),
		"no PATH":    environ("HOME=/fixture"),
		"empty name": environ("PATH=/usr/bin", "=value"),
	} {
		if _, err := callIDVariable(capture, id); err == nil {
			t.Errorf("%s capture accepted", name)
		}
	}
}
