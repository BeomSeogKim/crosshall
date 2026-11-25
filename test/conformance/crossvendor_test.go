//go:build conformance

package conformance

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/hook"
)

const crossVendorChannel = "crossvendor"

type crossVendorStep struct {
	command string
	wants   []string
	askFrom string
}

type crossVendorCommands struct{ exe string }

func (c crossVendorCommands) command(args string, wants ...string) crossVendorStep {
	return crossVendorStep{command: codexQuote(c.exe) + " " + args, wants: wants}
}

func (c crossVendorCommands) ask(recipient, body string) crossVendorStep {
	return c.command("ask @"+recipient+" "+codexQuote(body), "stored. due")
}

func (c crossVendorCommands) readAsk(sender, body string) crossVendorStep {
	step := c.command("read", body)
	step.askFrom = sender
	return step
}

func (c crossVendorCommands) reply(recipient, body string) crossVendorStep {
	return c.command("reply {ask} "+codexQuote(body), "answers #{ask} (to @"+recipient+")")
}

func (c crossVendorCommands) readReply(sender, body string, askID int64) crossVendorStep {
	return c.command("read", body, fmt.Sprintf("reply (answer) from @%s in #%s (to #%d)", sender, crossVendorChannel, askID))
}

func crossVendorMessageID(ctx context.Context, db *sql.DB, kind, body string) (int64, error) {
	var id int64
	err := db.QueryRowContext(ctx, "SELECT id FROM message WHERE kind = ? AND body = ?", kind, body).Scan(&id)
	return id, err
}

type crossVendorModel struct {
	vendor              string
	mu                  sync.Mutex
	steps               []crossVendorStep
	prompt              string
	request, sequence   int
	lastCall, requestID string
	failure             error
	firstRequest        func(context.Context, []byte) (string, error)
}

func (m *crossVendorModel) begin(prompt string, steps []crossVendorStep) {
	m.beginWithFirstRequest(prompt, steps, nil)
}

func (m *crossVendorModel) beginWithFirstRequest(prompt string, steps []crossVendorStep, check func(context.Context, []byte) (string, error)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.prompt, m.steps, m.firstRequest = prompt, steps, check
	m.request, m.lastCall, m.requestID, m.failure = 0, "", "", nil
}

func (m *crossVendorModel) verify(t *testing.T) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failure != nil || m.request != len(m.steps)+1 {
		t.Fatalf("%s model got %d requests for %d tool calls: %v", m.vendor, m.request, len(m.steps), m.failure)
	}
}

func (m *crossVendorModel) checkRequest(ctx context.Context, raw []byte) error {
	if m.request == 0 {
		if m.firstRequest != nil {
			id, err := m.firstRequest(ctx, raw)
			m.requestID = id
			return err
		}
		if m.prompt == "" {
			return nil
		}
		markers, err := codexCounts(raw, map[string]string{"pending": m.prompt})
		if err != nil {
			return err
		}
		line := regexp.MustCompile(regexp.QuoteMeta(m.prompt) + `(?:\\n|")`)
		if markers["developer"]["pending"] != 1 || markers["user"]["pending"] != 0 || !line.Match(raw) {
			return fmt.Errorf("missing Codex developer prompt line %q: %v", m.prompt, markers)
		}
		return nil
	}
	if m.request > len(m.steps) {
		return fmt.Errorf("extra model request after turn end")
	}
	previous := m.steps[m.request-1]
	for _, want := range previous.wants {
		want = strings.ReplaceAll(want, "{ask}", m.requestID)
		if ok, result := returnedMarker(raw, m.lastCall, want); !ok {
			return fmt.Errorf("tool call %s missing %q: %s", m.lastCall, want, result)
		}
	}
	if previous.askFrom != "" {
		_, result := returnedMarker(raw, m.lastCall, previous.askFrom)
		match := regexp.MustCompile(`#([1-9][0-9]*) ask from @` + regexp.QuoteMeta(previous.askFrom) + `\b`).FindStringSubmatch(result)
		if match == nil {
			return fmt.Errorf("missing ask ID from @%s in tool result: %s", previous.askFrom, result)
		}
		if m.requestID != "" && m.requestID != match[1] {
			return fmt.Errorf("read request #%s after delivery named #%s", match[1], m.requestID)
		}
		m.requestID = match[1]
	}
	return nil
}

func (m *crossVendorModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
	if err != nil || !json.Valid(raw) {
		http.Error(w, "invalid fixture request", http.StatusBadRequest)
		return
	}
	if err := m.checkRequest(r.Context(), raw); err != nil {
		m.failure = err
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	command := ""
	if m.request < len(m.steps) {
		command = strings.ReplaceAll(m.steps[m.request].command, "{ask}", m.requestID)
	}
	m.request++
	m.sequence++
	m.lastCall = fmt.Sprintf("call_crossvendor_%d", m.sequence)
	w.Header().Set("Content-Type", "text/event-stream")
	if m.vendor == "claude" {
		var request struct{ Model string }
		_ = json.Unmarshal(raw, &request)
		claudeFixtureReply(w, request.Model, m.lastCall, command)
	} else if command != "" {
		codexFixtureShell(w, m.lastCall, command)
	} else {
		fmt.Fprintf(w, codexResponseStream, m.sequence)
	}
}

type crossVendorSession struct {
	model       *crossVendorModel
	prefix, env []string
	repo, id    string
	appServer   *codexSession
}

const crossVendorPrompt = "Run the fixture turn."

func (s *crossVendorSession) startTurn(t *testing.T, pending string, steps ...crossVendorStep) func() {
	t.Helper()
	s.model.begin(pending, steps)
	if s.appServer != nil {
		return func() {
			t.Helper()
			s.runCodexTurn(t)
			s.model.verify(t)
		}
	}

	args := append(slices.Clone(s.prefix[1:]), "-p", "--model", "haiku", "--output-format", "stream-json", "--verbose", "--tools", "Bash", "--allowedTools", "Bash")
	if s.id != "" {
		args = append(args, "--resume", s.id)
	}
	args = append(args, "--", crossVendorPrompt)
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	cmd := exec.CommandContext(ctx, s.prefix[0], args...)
	var stdout, stderr bytes.Buffer
	cmd.Dir, cmd.Env, cmd.Stdout, cmd.Stderr = s.repo, s.env, &stdout, &stderr
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatalf("start %s: %v", s.model.vendor, err)
	}
	var runErr error
	finish := sync.OnceFunc(func() { runErr = cmd.Wait() })
	t.Cleanup(func() {
		cancel()
		finish()
	})
	return func() {
		t.Helper()
		finish()
		cancel()
		if runErr != nil {
			t.Fatalf("%s turn: %v\n%.2000s\n%.2000s", s.model.vendor, runErr, stdout.String(), stderr.String())
		}
		s.model.verify(t)
		var session string
		completed := false
		for _, line := range bytes.Split(bytes.TrimSpace(stdout.Bytes()), []byte("\n")) {
			var event struct {
				Type, Subtype, Result string
				Session               string `json:"session_id"`
			}
			codexMust(t, json.Unmarshal(line, &event))
			if event.Session != "" {
				session = event.Session
			}
			completed = completed || (event.Type == "result" && event.Subtype == "success" && strings.TrimSpace(event.Result) == "OK")
		}
		if !completed || session == "" || (s.id != "" && s.id != session) {
			t.Fatalf("%s did not finish in the expected session: session %q, previous %q, completed %v\n%.1500s",
				s.model.vendor, session, s.id, completed, stdout.String())
		}
		s.id = session
	}
}

func (s *crossVendorSession) runCodexTurn(t *testing.T) {
	t.Helper()
	appServer := s.appServer
	request := appServer.send(t, "turn/start", map[string]any{
		"threadId": s.id,
		"input":    []map[string]any{{"type": "text", "text": crossVendorPrompt, "text_elements": []string{}}},
	})
	turnID, answered := "", false
	for {
		event := appServer.read(t)
		if event.ID == request {
			turnID = event.Result.Turn.ID
		}
		if event.Params.ThreadID != s.id {
			continue
		}
		switch event.Method {
		case "item/completed":
			answered = answered || (event.Params.Item.Type == "agentMessage" && event.Params.Item.Text == "OK")
		case "turn/completed":
			if turnID == "" || event.Params.Turn.ID != turnID || event.Params.Turn.Status != "completed" || !answered {
				s.model.verify(t)
				t.Fatalf("Codex did not finish the requested turn: %+v", event)
			}
			return
		}
	}
}

func TestCrossVendorRoundTrip(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	codexMust(t, err)
	codexMust(t, os.Chmod(root, 0700))
	env := codexEnv(t, root)
	state := filepath.Join(root, "state")
	repo := filepath.Join(root, "checkout")
	codexMust(t, os.Mkdir(repo, 0700))
	git := exec.Command("git", "init", "-q", repo)
	git.Env = env
	codexMust(t, git.Run())
	exe := codexBinary(t, root)

	codexModel := &crossVendorModel{vendor: "codex"}
	claudeModel := &crossVendorModel{vendor: "claude"}
	mux := http.NewServeMux()
	mux.Handle("POST /v1/responses", codexModel)
	mux.Handle("POST /v1/messages", claudeModel)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	endpoint, err := url.Parse(server.URL)
	codexMust(t, err)

	config := fmt.Sprintf("model_provider=\"fixture\"\nmodel=\"fixture\"\n[shell_environment_policy]\ninherit=\"all\"\n[model_providers.fixture]\nname=\"Fixture\"\nwire_api=\"responses\"\nbase_url=%q\n", server.URL+"/v1")
	codexPut(t, filepath.Join(root, "codex", "config.toml"), []byte(config))
	startCodexDispatcher(t, exe, state, env)
	codexMust(t, codexCLI(exe, env, "owner", "channel", "create", crossVendorChannel))
	codexMust(t, codexCLI(exe, env, "owner", "channel", "create", "lobby"))
	codexMust(t, codexCLI(exe, env, "owner", "channel", "default", "lobby", "--repo", repo))
	codexMust(t, codexCLI(exe, env, "owner", "publish", "hall", "off"))

	launch := func(model *crossVendorModel) *crossVendorSession {
		t.Helper()
		bin, err := exec.LookPath(model.vendor)
		if err != nil {
			t.Fatalf("the cross-vendor gate requires %s on PATH: %v", model.vendor, err)
		}
		prefix := codexToolCommand(t, bin, env, endpoint.Port(), root)
		vendorEnv := slices.Clone(env)
		if model.vendor == "claude" {
			home := filepath.Join(root, "claude-home")
			tmp := filepath.Join(home, "tmp")
			codexMust(t, os.MkdirAll(tmp, 0700))
			vendorEnv = slices.DeleteFunc(vendorEnv, func(kv string) bool {
				key, _, _ := strings.Cut(kv, "=")
				return key == "HOME" || key == "CODEX_HOME" || key == "TMPDIR"
			})
			vendorEnv = append(vendorEnv, "HOME="+home, "TMPDIR="+tmp, "CLAUDE_CODE_TMPDIR="+tmp,
				"ANTHROPIC_BASE_URL="+server.URL, "ANTHROPIC_API_KEY=sk-ant-fixture-not-a-key", "DISABLE_TELEMETRY=1",
				"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_AUTOUPDATER=1")
		}
		version := exec.Command(prefix[0], append(slices.Clone(prefix[1:]), "--version")...)
		version.Env, version.Dir = vendorEnv, repo
		out, err := version.CombinedOutput()
		if err != nil {
			t.Fatalf("%s --version: %v\n%s", model.vendor, err, out)
		}
		t.Logf("%s: %s", model.vendor, strings.TrimSpace(string(out)))
		return &crossVendorSession{model: model, prefix: prefix, env: vendorEnv, repo: repo}
	}

	for _, spec := range hook.InstallSpecs() {
		if spec.Adapter == "claude" || spec.Adapter == "codex" {
			world := measurementWorld{root: root, exe: exe, spec: spec}
			world.configure(t, "base")
			codexMust(t, codexCLI(exe, env, "owner", "adapter", "set", spec.Adapter, "--enabled", "on"))
		}
	}

	claude, codex := launch(claudeModel), launch(codexModel)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	t.Cleanup(cancel)
	codex.appServer = startCodexAppServer(t, ctx, &measurementWorld{
		root: root, repo: repo, exe: exe, env: codex.env, prefix: codex.prefix,
	})
	codex.id = codex.appServer.thread

	commands := crossVendorCommands{exe}
	for _, session := range []*crossVendorSession{claude, codex} {
		alias := session.model.vendor
		session.startTurn(t, "", commands.command("join "+crossVendorChannel+" --move --as "+alias, "Joined #"+crossVendorChannel+" as @"+alias))()
	}
	db := openCodexLedger(t, state)
	const (
		claudeAsk    = "Claude cross-vendor request payload"
		codexAnswer  = "Codex cross-vendor answer payload"
		codexAsk     = "Codex cross-vendor request payload"
		claudeAnswer = "Claude cross-vendor answer payload"
	)
	requestID := func(t *testing.T, body string) int64 {
		t.Helper()
		id, err := crossVendorMessageID(t.Context(), db, "ask", body)
		codexMust(t, err)
		return id
	}

	if !t.Run("claude_to_codex_next_prompt", func(t *testing.T) {
		send := commands.ask("codex", claudeAsk)
		send.wants = append(send.wants, "it sees this at its next prompt")
		claude.startTurn(t, "", send)()
		id := requestID(t, claudeAsk)
		codex.startTurn(t, fmt.Sprintf("[crosshall] pending: %d", id), commands.readAsk("claude", claudeAsk), commands.reply("claude", codexAnswer))()
		var offerReasons string
		codexMust(t, db.QueryRowContext(t.Context(), `SELECT COALESCE(GROUP_CONCAT(o.reason), '')
			FROM offer_item i JOIN offer o ON o.id = i.offer_id
			JOIN principal_key k ON k.principal_id = i.principal_id
			WHERE i.message_id = ? AND k.vendor = 'codex' AND k.key = ? AND o.written_at_ms IS NOT NULL`,
			id, codex.id).Scan(&offerReasons))
		if offerReasons != "prompt" {
			t.Fatalf("Codex request #%d: want one confirmed prompt offer, got %q", id, offerReasons)
		}
		claude.startTurn(t, "", commands.readReply("codex", codexAnswer, id))()
		assertCrossVendorSettlement(t, db, claude, codex, id, claudeAsk, codexAnswer)
	}) {
		return
	}

	t.Run("codex_to_claude_running_wait", func(t *testing.T) {
		wait := commands.command("wait --existing --work-only --timeout 35s", "Arrived during the wait")
		wait.askFrom = "codex"
		finish := claude.startTurn(t, "", wait, commands.readAsk("codex", codexAsk), commands.reply("codex", claudeAnswer))
		rosterWait := crossVendorStep{
			command: fmt.Sprintf(`i=0
			while [ "$i" -lt 50 ]; do
				roster=$(%s who) || exit 1
				printf '%%s\n' "$roster" | grep -E '^[[:space:]]*@claude[[:space:]].*woken by its wait$' && exit 0
				i=$((i + 1))
				sleep 0.1
			done
			printf '%%s\n' "$roster"
			exit 1`, codexQuote(exe)),
			wants: []string{"woken by its wait"},
		}
		send := commands.ask("claude", codexAsk)
		send.wants = append(send.wants, "its wait is running")
		codex.startTurn(t, "", rosterWait, send)()
		finish()
		id := requestID(t, codexAsk)
		codex.startTurn(t, "", commands.readReply("claude", claudeAnswer, id))()
		assertCrossVendorSettlement(t, db, codex, claude, id, codexAsk, claudeAnswer)
	})
}

func assertCrossVendorSettlement(t *testing.T, db *sql.DB, asker, answerer *crossVendorSession, askID int64, ask, answer string) {
	t.Helper()
	var matched int
	codexMust(t, db.QueryRowContext(t.Context(), `SELECT COUNT(*)
		FROM message a
		JOIN principal_key ak ON ak.principal_id = a.sender_id
		JOIN recipient ar ON ar.message_id = a.id
		JOIN obligation o ON o.message_id = a.id AND o.principal_id = ar.principal_id
		JOIN message r ON r.id = o.settlement_message_id AND r.in_reply_to = a.id AND r.sender_id = ar.principal_id
		JOIN principal_key rk ON rk.principal_id = r.sender_id
		JOIN recipient rr ON rr.message_id = r.id AND rr.principal_id = a.sender_id
		WHERE a.id = ? AND a.kind = 'ask' AND a.body = ?
		  AND ak.vendor = ? AND ak.key = ? AND rk.vendor = ? AND rk.key = ?
		  AND r.kind = 'reply' AND r.outcome = 'answer' AND r.body = ?
		  AND ar.picked_up_at_ms IS NOT NULL AND o.state = 'replied' AND o.settled_at_ms >= ar.picked_up_at_ms
		  AND rr.picked_up_at_ms IS NOT NULL
		  AND (SELECT COUNT(*) FROM message WHERE kind = 'ask' AND body = a.body) = 1
		  AND (SELECT COUNT(*) FROM message WHERE in_reply_to = a.id) = 1`,
		askID, ask, asker.model.vendor, asker.id, answerer.model.vendor, answerer.id, answer).Scan(&matched))
	if matched != 1 {
		t.Fatalf("request #%d lacks a single picked-up and settled native %s → %s round trip",
			askID, asker.model.vendor, answerer.model.vendor)
	}
}
