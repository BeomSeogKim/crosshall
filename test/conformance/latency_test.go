//go:build conformance

package conformance

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/hook"
	"github.com/BeomSeogKim/crosshall/internal/ident"
)

// The fixture drives exactly one successful, sequential shell call per model
// round. The output marker is assembled by printf, so a denied command or a
// tool description alone cannot satisfy the next request's result check.
type turnModel struct {
	mu                    sync.Mutex
	vendor                string
	turn, steps, requests int
	failure               string
}

func (m *turnModel) begin(steps int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.turn++
	m.steps, m.requests, m.failure = steps, 0, ""
}

func (m *turnModel) verify(t *testing.T) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failure != "" || m.requests != m.steps+1 {
		t.Fatalf("%s fixture: %d requests for %d calls: %s", m.vendor, m.requests, m.steps, m.failure)
	}
}

func event(w io.Writer, name string, body any) {
	raw, _ := json.Marshal(body)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", name, raw)
}

// Match only a result of the actual preceding call, outside the tool schema,
// conversation history, prompt, tool arguments and either vendor's envelope.
func returnedMarker(raw []byte, call, marker string) (bool, string) {
	var req struct {
		Input []struct {
			Type   string
			CallID string `json:"call_id"`
			Output json.RawMessage
		}
		Messages []struct{ Content json.RawMessage }
	}
	if json.Unmarshal(raw, &req) != nil {
		return false, "no matching tool result"
	}
	for _, part := range req.Input {
		if part.Type == "function_call_output" && part.CallID == call {
			return bytes.Contains(part.Output, []byte(marker)), fmt.Sprintf("%.600s", part.Output)
		}
	}
	for _, msg := range req.Messages {
		var parts []struct {
			Type    string
			ToolID  string `json:"tool_use_id"`
			Content json.RawMessage
			IsError bool `json:"is_error"`
		}
		if json.Unmarshal(msg.Content, &parts) != nil {
			continue
		}
		for _, part := range parts {
			if part.Type == "tool_result" && part.ToolID == call {
				return !part.IsError && bytes.Contains(part.Content, []byte(marker)), fmt.Sprintf("%.600s", part.Content)
			}
		}
	}
	return false, "no matching tool result"
}

func (m *turnModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	claude := m.vendor == "claude"
	if r.Method != http.MethodPost || (claude && r.URL.Path != "/v1/messages") || (!claude && r.URL.Path != "/v1/responses") {
		http.NotFound(w, r)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
	if err != nil || !json.Valid(raw) {
		http.Error(w, "invalid fixture request", http.StatusBadRequest)
		return
	}
	previous := fmt.Sprintf("%d_%d", m.turn, m.requests)
	if m.requests > 0 && m.failure == "" {
		if ok, detail := returnedMarker(raw, "call_latency_"+previous, "crosshalllatency"+previous); !ok {
			m.failure = fmt.Sprintf("missing successful result for call %d: %s", m.requests, detail)
		}
	}
	m.requests++
	w.Header().Set("Content-Type", "text/event-stream")
	if !claude && m.requests > m.steps {
		fmt.Fprintf(w, codexResponseStream, m.requests)
		return
	}
	suffix := fmt.Sprintf("%d_%d", m.turn, m.requests)
	id := "call_latency_" + suffix
	cmd := fmt.Sprintf("printf 'cross%%s%%s' hall latency%s", suffix)
	if !claude {
		codexFixtureShell(w, id, cmd)
		return
	}
	var request struct{ Model string }
	_ = json.Unmarshal(raw, &request)
	if m.requests > m.steps {
		cmd = ""
	}
	claudeFixtureReply(w, request.Model, id, cmd)
}

func codexFixtureShell(w io.Writer, id, command string) {
	codexFixtureExec(w, id, map[string]any{"cmd": command, "login": false})
}

func codexFixtureExec(w io.Writer, id string, arguments map[string]any) {
	args, _ := json.Marshal(arguments)
	call := map[string]any{"type": "function_call", "name": "exec_command", "call_id": id, "id": id, "arguments": string(args), "status": "completed"}
	event(w, "response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_" + id, "model": "fixture", "status": "in_progress", "output": []any{}}})
	event(w, "response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": 0, "item": call})
	event(w, "response.completed", map[string]any{"type": "response.completed", "response": map[string]any{"id": "resp_" + id, "model": "fixture", "status": "completed", "output": []any{call}}})
}

func claudeFixtureReply(w io.Writer, model, id, command string) {
	event(w, "message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_" + id, "type": "message", "role": "assistant", "model": model, "content": []any{}, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}}})
	var block, delta map[string]any
	stop := "end_turn"
	if command != "" {
		stop = "tool_use"
		args, _ := json.Marshal(map[string]string{"command": command})
		block = map[string]any{"type": "tool_use", "id": id, "name": "Bash", "input": map[string]any{}}
		delta = map[string]any{"type": "input_json_delta", "partial_json": string(args)}
	} else {
		block = map[string]any{"type": "text", "text": ""}
		delta = map[string]any{"type": "text_delta", "text": "OK"}
	}
	event(w, "content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": block})
	event(w, "content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": delta})
	event(w, "content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	event(w, "message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop, "stop_sequence": nil}, "usage": map[string]int{"output_tokens": 1}})
	event(w, "message_stop", map[string]any{"type": "message_stop"})
}

type measurementWorld struct {
	root, repo, exe string
	env, prefix     []string
	spec            hook.InstallSpec
	model           *turnModel
}

// Read the stable hook counter only after the vendor exits. Sampling does not
// insert a wrapper, probe or timer into the hook's foreground path.
func (w *measurementWorld) counters(t *testing.T) []string {
	t.Helper()
	dir := filepath.Join(ident.Rendezvous(filepath.Join(w.root, "state"), os.Geteuid()), "hooks")
	files, err := filepath.Glob(filepath.Join(dir, "????????"))
	codexMust(t, err)
	var lines []string
	for _, file := range files {
		raw, err := os.ReadFile(file)
		codexMust(t, err)
		lines = append(lines, strings.FieldsFunc(string(raw), func(r rune) bool { return r == '\n' })...)
	}
	return lines
}

// The post arm adds a vendor matcher only in the fixture home. Its target
// deliberately names an undefined hook. This times launching the real binary
// and its normal refusal path without exercising receipt processing.
const measurementPostEvent = "MeasurementPostToolProbe"

func (w *measurementWorld) configure(t *testing.T, mode string) {
	t.Helper()
	groups := map[string][]any{}
	if mode != "off" {
		events := slices.Clone(w.spec.Events)
		if mode == "post-probe" {
			events = append(events, "PostToolUse")
		}
		for _, name := range events {
			target := name
			if name == "PostToolUse" {
				target = measurementPostEvent
			}
			handler := map[string]any{"type": "command", "timeout": w.spec.Timeout, "command": w.exe, "args": []string{"hook", w.spec.Adapter, target}}
			matcher := w.spec.Matchers[name]
			if w.spec.Encoding == "shell" {
				delete(handler, "args")
				handler["command"] = codexQuote(w.exe) + " hook " + w.spec.Adapter + " " + target
			}
			if name == "PostToolUse" {
				matcher = "^Bash$"
				if w.spec.Adapter == "claude" {
					matcher = "Bash"
					handler["if"] = "Bash(crosshall read *)"
				}
			}
			group := map[string]any{"hooks": []any{handler}}
			if matcher != "" {
				group["matcher"] = matcher
			}
			groups[name] = []any{group}
		}
	}
	config, err := json.Marshal(map[string]any{"hooks": groups})
	codexMust(t, err)
	codexPut(t, filepath.Join(w.root, w.spec.Adapter, w.spec.File), config)
}

type observation struct {
	elapsed            time.Duration
	starts, postStarts int
	hookMS, postMS     []int
}

func (w *measurementWorld) run(t *testing.T, id string, steps int) (string, observation) {
	t.Helper()
	w.model.begin(steps)
	before := len(w.counters(t))
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	args := slices.Clone(w.prefix[1:])
	if w.spec.Adapter == "claude" {
		args = append(args, "-p", "--model", "haiku", "--output-format", "stream-json", "--verbose", "--tools", "Bash", "--allowedTools", "Bash(printf:*)")
		if id != "" {
			args = append(args, "--resume", id)
		}
		args = append(args, "--", "Run this fixture turn.")
	} else {
		args = append(args, "-a", "never", "exec", "--dangerously-bypass-hook-trust", "--skip-git-repo-check", "--sandbox", codexToolSandbox(), "--json")
		if id != "" {
			args = append(args, "resume", id)
		}
		args = append(args, "Run this fixture turn.")
	}
	cmd := exec.CommandContext(ctx, w.prefix[0], args...)
	cmd.Env, cmd.Dir = w.env, w.repo
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	codexMust(t, err)
	start := time.Now()
	codexMust(t, cmd.Start())
	result := observation{}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		var msg struct {
			Type, Subtype, Result string
			Session               string `json:"session_id"`
			Thread                string `json:"thread_id"`
		}
		codexMust(t, json.Unmarshal(sc.Bytes(), &msg))
		if msg.Thread != "" {
			id = msg.Thread
		}
		if msg.Session != "" {
			id = msg.Session
		}
		if msg.Type == "turn.completed" || (msg.Type == "result" && msg.Subtype == "success" && strings.TrimSpace(msg.Result) == "OK") {
			result.elapsed = time.Since(start)
		}
	}
	codexMust(t, sc.Err())
	if err := cmd.Wait(); err != nil || result.elapsed == 0 || id == "" {
		t.Fatalf("%s turn: %v, result=%s, session=%q: %.1500s", w.spec.Adapter, err, result.elapsed, id, stderr.String())
	}
	w.model.verify(t)
	for _, line := range w.counters(t)[before:] {
		fields := strings.Fields(line)
		if len(fields) != 4 || fields[0] != w.spec.Adapter {
			t.Fatalf("invalid fixture hook counter %q", line)
		}
		ms, err := strconv.Atoi(fields[3])
		codexMust(t, err)
		result.starts++
		result.hookMS = append(result.hookMS, ms)
		if fields[1] == "unknown" {
			if fields[2] != "refused" {
				t.Fatalf("unexpected outcome for the fixture-only callback: %q", line)
			}
			result.postStarts++
			result.postMS = append(result.postMS, ms)
		}
	}
	return id, result
}

func makeMeasurementWorld(t *testing.T, spec hook.InstallSpec, observe func(http.Handler) http.Handler) *measurementWorld {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	codexMust(t, err)
	codexMust(t, os.Chmod(root, 0700))
	env := codexEnv(t, root)
	repo := filepath.Join(root, "repo")
	codexMust(t, os.Mkdir(repo, 0700))
	git := exec.Command("git", "init", "-q", repo)
	git.Env = env
	codexMust(t, git.Run())
	exe := codexBinary(t, root)
	model := &turnModel{vendor: spec.Adapter}
	var handler http.Handler = model
	if observe != nil {
		handler = observe(handler)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	endpoint, err := url.Parse(server.URL)
	codexMust(t, err)
	bin, err := exec.LookPath(spec.Adapter)
	codexMust(t, err)
	prefix := codexToolCommand(t, bin, env, endpoint.Port(), root)
	if spec.Adapter == "codex" {
		config := fmt.Sprintf("model_provider=\"fixture\"\nmodel=\"fixture\"\n[model_providers.fixture]\nname=\"Fixture\"\nwire_api=\"responses\"\nbase_url=%q\n", server.URL+"/v1")
		codexPut(t, filepath.Join(root, "codex", "config.toml"), []byte(config))
	} else {
		env = append(env, "CLAUDE_CODE_TMPDIR="+filepath.Join(root, "tmp"), "ANTHROPIC_BASE_URL="+server.URL, "ANTHROPIC_API_KEY=sk-ant-fixture-not-a-key", "DISABLE_TELEMETRY=1", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_AUTOUPDATER=1")
	}
	startCodexDispatcher(t, exe, filepath.Join(root, "state"), env)
	codexMust(t, codexCLI(exe, env, "owner", "channel", "create", "latency"))
	codexMust(t, codexCLI(exe, env, "owner", "channel", "default", "latency", "--repo", repo))
	codexMust(t, codexCLI(exe, env, "owner", "adapter", "set", spec.Adapter, "--enabled", "on"))
	ver := exec.Command(prefix[0], append(slices.Clone(prefix[1:]), "--version")...)
	ver.Dir, ver.Env = repo, env
	raw, err := ver.Output()
	codexMust(t, err)
	t.Logf("vendor %s, %s", spec.Adapter, strings.TrimSpace(string(raw)))
	return &measurementWorld{root, repo, exe, env, prefix, spec, model}
}

func rank(values []int, percent int) int {
	slices.Sort(values)
	return values[(len(values)*percent+99)/100-1]
}

func TestPairedTurnLatency(t *testing.T) {
	raw := os.Getenv("CROSSHALL_TURN_PAIRS")
	if raw == "" {
		t.Skip("run scripts/measure-turns.sh")
	}
	pairs, err := strconv.Atoi(raw)
	if err != nil || pairs < 1 {
		t.Fatal("CROSSHALL_TURN_PAIRS must be a positive integer")
	}
	t.Log("post-probe is a fixture-only PostToolUse registration; the callback opens the normal counter and refuses an undefined event")
	for _, spec := range hook.InstallSpecs() {
		t.Run(spec.Adapter, func(t *testing.T) {
			w := makeMeasurementWorld(t, spec, nil)
			modes := []string{"off", "base", "post-probe"}
			for _, steps := range []int{0, 10} {
				ids := map[string]string{}
				w.configure(t, "base")
				for _, mode := range modes {
					ids[mode], _ = w.run(t, "", 0)
				}
				for _, mode := range modes {
					w.configure(t, mode)
					w.run(t, ids[mode], 0)
				}
				baseUS, postUS, extraUS := []int{}, []int{}, []int{}
				var hookMS, postMS []int
				for i := 0; i < pairs; i++ {
					samples := map[string]observation{}
					for j := range modes {
						k := (i + j) % len(modes)
						if i%2 != 0 {
							k = len(modes) - 1 - k
						}
						mode := modes[k]
						w.configure(t, mode)
						_, sample := w.run(t, ids[mode], steps)
						if mode == "off" && sample.starts != 0 {
							t.Fatal("hooks fired in the off baseline")
						}
						if mode != "off" && sample.starts < 2 {
							t.Fatalf("%s: missing ordinary hook starts", mode)
						}
						wantPost := 0
						if spec.Adapter == "codex" && mode == "post-probe" {
							wantPost = steps
						}
						if sample.postStarts != wantPost {
							t.Fatalf("%s: %d post-tool callbacks, want %d", mode, sample.postStarts, wantPost)
						}
						samples[mode] = sample
						hookMS = append(hookMS, sample.hookMS...)
						postMS = append(postMS, sample.postMS...)
					}
					us := func(a, b string) int { return int((samples[a].elapsed - samples[b].elapsed).Microseconds()) }
					baseUS = append(baseUS, us("base", "off"))
					postUS = append(postUS, us("post-probe", "off"))
					extraUS = append(extraUS, us("post-probe", "base"))
					t.Logf("pair vendor=%s tools=%d n=%d requests=%d continuations=0 off_us=%d base_us=%d post_probe_us=%d starts=%d/%d/%d post_probe_starts=%d", spec.Adapter, steps, i+1, steps+1, samples["off"].elapsed.Microseconds(), samples["base"].elapsed.Microseconds(), samples["post-probe"].elapsed.Microseconds(), samples["off"].starts, samples["base"].starts, samples["post-probe"].starts, samples["post-probe"].postStarts)
				}
				for _, delta := range []struct {
					name string
					us   []int
				}{{"base-off", baseUS}, {"post-probe-off", postUS}, {"post-probe-base", extraUS}} {
					t.Logf("delta vendor=%s tools=%d comparison=%s pairs=%d p50_ms=%.3f p95_ms=%.3f", spec.Adapter, steps, delta.name, pairs, float64(rank(delta.us, 50))/1000, float64(rank(delta.us, 95))/1000)
				}
				if steps > 0 {
					t.Logf("post_probe_per_tool vendor=%s tools=%d pairs=%d p50_ms=%.3f p95_ms=%.3f", spec.Adapter, steps, pairs, float64(rank(extraUS, 50))/float64(steps*1000), float64(rank(extraUS, 95))/float64(steps*1000))
				}
				t.Logf("handler vendor=%s tools=%d observations=%d p50_ms=%d p95_ms=%d", spec.Adapter, steps, len(hookMS), rank(hookMS, 50), rank(hookMS, 95))
				if len(postMS) > 0 {
					t.Logf("post_probe_refusal vendor=%s observations=%d p50_ms=%d p95_ms=%d", spec.Adapter, len(postMS), rank(postMS, 50), rank(postMS, 95))
				}
			}
		})
	}
}
