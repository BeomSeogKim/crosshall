//go:build conformance

package conformance

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/hook"
)

// The persistent app-server uses its native stdio RPC. A turn begins at the
// client's submission and ends at its matching turn/completed notification.
// No clock, wrapper, or observer process is added to the vendor hook command.
type codexSession struct {
	world  *measurementWorld
	input  *json.Encoder
	output *json.Decoder
	thread string
	id     int
	hookUS map[string][]int
}

type sessionMessage struct {
	ID     int
	Method string
	Error  *struct {
		Code    int
		Message string
	}
	Result struct {
		Thread struct{ ID string }
		Turn   struct{ ID string }
	}
	Params struct {
		ThreadID string
		Turn     struct{ ID, Status string }
		Item     struct{ Type, Text string }
		Run      struct{ EventName string }
	}
}

func (s *codexSession) send(t *testing.T, method string, params any) int {
	t.Helper()
	s.id++
	codexMust(t, s.input.Encode(map[string]any{"id": s.id, "method": method, "params": params}))
	return s.id
}

func (s *codexSession) read(t *testing.T) sessionMessage {
	t.Helper()
	var m sessionMessage
	codexMust(t, s.output.Decode(&m))
	if m.Error != nil {
		t.Fatalf("app-server RPC failed: %d %s", m.Error.Code, m.Error.Message)
	}
	return m
}

func (s *codexSession) call(t *testing.T, method string, params any) sessionMessage {
	t.Helper()
	id := s.send(t, method, params)
	for {
		if m := s.read(t); m.ID == id {
			return m
		}
	}
}

func startLatencySession(t *testing.T, w *measurementWorld) *codexSession {
	t.Helper()
	return startCodexAppServer(t, t.Context(), w)
}

func startCodexAppServer(t *testing.T, ctx context.Context, w *measurementWorld) *codexSession {
	t.Helper()
	args := append(slices.Clone(w.prefix[1:]), "-c", "bypass_hook_trust=true", "app-server")
	cmd := exec.CommandContext(ctx, w.prefix[0], args...)
	cmd.Env, cmd.Dir = w.env, w.repo
	in, err := cmd.StdinPipe()
	codexMust(t, err)
	out, err := cmd.StdoutPipe()
	codexMust(t, err)
	codexMust(t, cmd.Start())
	t.Cleanup(func() {
		_ = in.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	s := &codexSession{world: w, input: json.NewEncoder(in), output: json.NewDecoder(out)}
	s.call(t, "initialize", map[string]any{
		"clientInfo":   map[string]string{"name": "crosshall_fixture", "version": "1"},
		"capabilities": map[string]bool{"experimentalApi": true},
	})
	codexMust(t, s.input.Encode(map[string]any{"method": "initialized", "params": map[string]any{}}))
	s.thread = s.call(t, "thread/start", map[string]any{
		"cwd": w.repo, "approvalPolicy": "never", "sandbox": codexToolSandbox(),
		"config": map[string]bool{"bypass_hook_trust": true},
	}).Result.Thread.ID
	if s.thread == "" {
		t.Fatal("missing app-server thread id")
	}
	return s
}

func (s *codexSession) turn(t *testing.T, steps int, measured bool, on bool) time.Duration {
	t.Helper()
	s.world.model.begin(steps)
	start := time.Now()
	id := s.send(t, "turn/start", map[string]any{"threadId": s.thread,
		"input": []map[string]any{{"type": "text", "text": "Run this fixture turn.", "text_elements": []string{}}}})
	turnID, gotOK := "", false
	started := map[string]int{}
	spans := map[string]time.Time{}
	for {
		m := s.read(t)
		if m.ID == id {
			turnID = m.Result.Turn.ID
		}
		if m.Params.ThreadID != s.thread {
			continue
		}
		switch m.Method {
		case "hook/started":
			started[m.Params.Run.EventName]++
			if s.hookUS != nil {
				spans[m.Params.Run.EventName] = time.Now()
			}
		case "hook/completed":
			if at, ok := spans[m.Params.Run.EventName]; ok {
				s.hookUS[m.Params.Run.EventName] = append(s.hookUS[m.Params.Run.EventName], int(time.Since(at).Microseconds()))
				delete(spans, m.Params.Run.EventName)
			}
		case "item/completed":
			if m.Params.Item.Type == "agentMessage" && m.Params.Item.Text == "OK" {
				gotOK = true
			}
		case "turn/completed":
			elapsed := time.Since(start)
			if turnID == "" || m.Params.Turn.ID != turnID || m.Params.Turn.Status != "completed" || !gotOK {
				t.Fatal("app-server turn did not deliver its matching OK")
			}
			s.world.model.verify(t)
			if measured {
				if on && (len(started) != 2 || started["userPromptSubmit"] != 1 || started["stop"] != 1) || !on && len(started) != 0 {
					t.Fatalf("unexpected quiet-turn native hook starts: %v", started)
				}
			}
			return elapsed
		}
	}
}

// Exec resumes need a process per turn. Install only the event(s) named by
// each control to attribute their added cost, even if stdout is buffered.
// Sample both the visible result and process exit; verify native hook counters
// after exit so the counter read itself cannot delay a measured boundary.
type codexExecPhases struct{ toResult, afterResult, total time.Duration }

func codexExecControl(t *testing.T, w *measurementWorld, thread string, events []string, pair int) codexExecPhases {
	t.Helper()
	plan := *w
	plan.spec.Events = events
	plan.configure(t, "base")
	w.model.begin(0)
	before := len(w.counters(t))
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()
	args := append(slices.Clone(w.prefix[1:]), "-a", "never", "exec", "--dangerously-bypass-hook-trust",
		"--skip-git-repo-check", "--sandbox", codexToolSandbox(), "--json", "resume", thread, "Run this fixture turn.")
	cmd := exec.CommandContext(ctx, w.prefix[0], args...)
	cmd.Env, cmd.Dir = w.env, w.repo
	out, err := cmd.StdoutPipe()
	codexMust(t, err)
	start := time.Now()
	codexMust(t, cmd.Start())
	defer func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	dec := json.NewDecoder(out)
	var end time.Time
	var gotThread, gotOK bool
	for {
		var m struct {
			Type   string
			Thread string `json:"thread_id"`
			Item   struct{ Type, Text string }
		}
		err := dec.Decode(&m)
		if err == io.EOF {
			break
		}
		codexMust(t, err)
		switch m.Type {
		case "thread.started":
			gotThread = m.Thread == thread
		case "item.completed":
			if m.Item.Type == "agent_message" && m.Item.Text == "OK" {
				gotOK = true
			}
		case "turn.completed":
			end = time.Now()
		}
	}
	codexMust(t, cmd.Wait())
	exit := time.Now()
	if !gotThread || !gotOK || end.IsZero() {
		t.Fatal("resumed exec did not deliver the expected thread and OK")
	}
	w.model.verify(t)
	codexSessionCounters(t, w.counters(t)[before:], events, pair)
	return codexExecPhases{end.Sub(start), exit.Sub(end), exit.Sub(start)}
}

func codexSessionCounters(t *testing.T, lines, want []string, pair int) {
	t.Helper()
	var got []string
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 4 || fields[0] != "codex" || (fields[2] != "rpc" && fields[2] != "clean") {
			t.Fatalf("Codex sample %d: unexpected hook outcome %q, want clean or rpc for %v; counters %q", pair, line, want, lines)
		}
		got = append(got, fields[1])
	}
	slices.Sort(got)
	if !slices.Equal(got, slices.Sorted(slices.Values(want))) {
		t.Fatalf("Codex sample %d: hooks ran %v, want %v; counters %q", pair, got, want, lines)
	}
}

// Equal-tailed paired bootstrap confidence intervals describe this local run.
// Each resampled unit is one time-adjacent on/off pair.
func codexLatencyStats(t *testing.T, name string, us []int) {
	t.Helper()
	rng := rand.New(rand.NewSource(216))
	resample := make([]int, len(us))
	var medians, tails []int
	for i := 0; i < 2000; i++ {
		for j := range resample {
			resample[j] = us[rng.Intn(len(us))]
		}
		medians = append(medians, rank(resample, 50))
		tails = append(tails, rank(resample, 95))
	}
	interval := func(values []int) (float64, float64) {
		slices.Sort(values)
		return float64(values[len(values)*25/1000-1]) / 1000, float64(values[len(values)*975/1000-1]) / 1000
	}
	lo50, hi50 := interval(medians)
	lo95, hi95 := interval(tails)
	t.Logf("codex_delta metric=%s pairs=%d p50_ms=%.3f ci95_p50=[%.3f,%.3f] p95_ms=%.3f ci95_p95=[%.3f,%.3f]",
		name, len(us), float64(rank(us, 50))/1000, lo50, hi50, float64(rank(us, 95))/1000, lo95, hi95)
}

func TestCodexSessionLatency(t *testing.T) {
	raw := os.Getenv("CROSSHALL_SESSION_PAIRS")
	if raw == "" {
		t.Skip("set CROSSHALL_SESSION_PAIRS for the persistent Codex session probe")
	}
	pairs, err := strconv.Atoi(raw)
	if err != nil || pairs < 1 {
		t.Fatal("expected positive CROSSHALL_SESSION_PAIRS")
	}
	var spec hook.InstallSpec
	for _, candidate := range hook.InstallSpecs() {
		if candidate.Adapter == "codex" {
			spec = candidate
		}
	}
	t.Run("app-server", func(t *testing.T) {
		var sessions [2]*codexSession
		for arm, mode := range []string{"off", "base"} {
			w := makeMeasurementWorld(t, spec, nil)
			w.configure(t, mode)
			sessions[arm] = startLatencySession(t, w)
			// Positive control: a sandboxed printf must return its marker to
			// the next actual model request before we time any quiet turn.
			sessions[arm].turn(t, 1, false, arm == 1)
			sessions[arm].turn(t, 0, true, arm == 1)
		}
		var delta []int
		sessions[1].hookUS = make(map[string][]int)
		for i := 0; i < pairs; i++ {
			var us [2]int
			for j := range sessions {
				k := (i + j) % 2
				s := sessions[k]
				before := len(s.world.counters(t))
				us[k] = int(s.turn(t, 0, true, k == 1).Microseconds())
				var wanted []string
				if k == 1 {
					wanted = []string{"UserPromptSubmit", "Stop"}
				}
				codexSessionCounters(t, s.world.counters(t)[before:], wanted, i+1)
			}
			delta = append(delta, us[1]-us[0])
			t.Logf("codex_pair mode=app-server n=%d off_us=%d on_us=%d hooks=0/2 requests=1/1 outputs=OK/OK", i+1, us[0], us[1])
		}
		codexLatencyStats(t, "app-server.quiet", delta)
		for _, name := range []string{"userPromptSubmit", "stop"} {
			values := sessions[1].hookUS[name]
			if len(values) != pairs {
				t.Fatalf("expected %d complete native %s spans, got %d", pairs, name, len(values))
			}
			t.Logf("native_hook event=%s pairs=%d p50_ms=%.3f p95_ms=%.3f", name, pairs, float64(rank(values, 50))/1000, float64(rank(values, 95))/1000)
		}
	})
	t.Run("exec-resume", func(t *testing.T) {
		w := makeMeasurementWorld(t, spec, nil)
		modes := []struct {
			name   string
			events []string
		}{
			{"off", nil}, {"start", []string{"SessionStart"}}, {"end", []string{"SessionEnd"}},
			{"turn", []string{"UserPromptSubmit", "Stop"}},
		}
		var ids [4]string
		w.configure(t, "base")
		for i := range modes {
			ids[i], _ = w.run(t, "", 1)
		}
		for i, m := range modes {
			codexExecControl(t, w, ids[i], m.events, 0)
		}
		values := map[string][]int{}
		for i := 0; i < pairs; i++ {
			var arms [4]codexExecPhases
			for j := range modes {
				k := (j + i) % len(modes)
				if (i/len(modes))%2 == 1 {
					k = len(modes) - 1 - k
				}
				arms[k] = codexExecControl(t, w, ids[k], modes[k].events, i+1)
			}
			for k, m := range modes {
				a := arms[k]
				t.Logf("codex_pair mode=exec-resume arm=%s n=%d to_result_us=%d after_result_us=%d total_us=%d hooks=%d requests=1 output=OK", m.name, i+1, a.toResult.Microseconds(), a.afterResult.Microseconds(), a.total.Microseconds(), len(m.events))
				if k == 0 {
					continue
				}
				for _, phase := range []struct {
					name  string
					delta time.Duration
				}{
					{"to-result", a.toResult - arms[0].toResult}, {"after-result", a.afterResult - arms[0].afterResult}, {"total", a.total - arms[0].total},
				} {
					key := fmt.Sprintf("exec-resume.%s.%s", m.name, phase.name)
					values[key] = append(values[key], int(phase.delta.Microseconds()))
				}
			}
		}
		for _, m := range modes[1:] {
			for _, phase := range []string{"to-result", "after-result", "total"} {
				key := "exec-resume." + m.name + "." + phase
				codexLatencyStats(t, key, values[key])
			}
		}
	})
}
