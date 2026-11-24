//go:build conformance

package conformance

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/hook"
)

// Running-session latency: from the repository root, run
//
//	CROSSHALL_CLAUDE_LIVE_PAIRS=100 go test -count=1 -tags conformance -run '^TestClaudeRunningTurnLatency$' -v ./test/conformance
//
// The two arms stay open while five successive prompts are compared. Opening
// the next pair of sessions gives independent startup and shutdown samples.
type claudeRoundTrip struct {
	arrival, reply time.Time
	markers        int
}

type claudeLoopbackTiming struct {
	sync.Mutex
	trips []claudeRoundTrip
}

func (timing *claudeLoopbackTiming) observe(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			next.ServeHTTP(w, r)
			return
		}
		arrival := time.Now()
		var raw bytes.Buffer
		body := r.Body
		r.Body = io.NopCloser(io.TeeReader(body, &raw))
		defer body.Close()
		next.ServeHTTP(w, r)
		reply := time.Now()
		timing.Lock()
		timing.trips = append(timing.trips, claudeRoundTrip{arrival, reply, bytes.Count(raw.Bytes(), []byte("[crosshall]"))})
		timing.Unlock()
	})
}

func (timing *claudeLoopbackTiming) count() int {
	timing.Lock()
	defer timing.Unlock()
	return len(timing.trips)
}

func (timing *claudeLoopbackTiming) next(t *testing.T, index int) claudeRoundTrip {
	t.Helper()
	timing.Lock()
	defer timing.Unlock()
	if len(timing.trips) != index+1 {
		t.Fatalf("prompt produced %d loopback model requests", len(timing.trips)-index)
	}
	return timing.trips[index]
}

type liveClaude struct {
	world         *measurementWorld
	timing        *claudeLoopbackTiming
	cmd           *exec.Cmd
	in            io.WriteCloser
	out           *bufio.Scanner
	started, init time.Time
	cancel        context.CancelFunc
	stderr        bytes.Buffer
}

func openLiveClaude(t *testing.T, w *measurementWorld, timing *claudeLoopbackTiming) *liveClaude {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	args := append(slices.Clone(w.prefix[1:]), "-p", "--model", "haiku", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--tools", "")
	cmd := exec.CommandContext(ctx, w.prefix[0], args...)
	cmd.Dir, cmd.Env = w.repo, w.env
	in, err := cmd.StdinPipe()
	codexMust(t, err)
	output, err := cmd.StdoutPipe()
	codexMust(t, err)
	out := bufio.NewScanner(output)
	out.Buffer(make([]byte, 64<<10), 4<<20)
	c := &liveClaude{world: w, timing: timing, cmd: cmd, in: in, out: out, cancel: cancel}
	cmd.Stderr = &c.stderr
	c.started = time.Now()
	codexMust(t, cmd.Start())
	t.Cleanup(func() {
		cancel()
		if cmd.ProcessState == nil {
			_ = in.Close()
			_ = cmd.Wait()
		}
	})
	return c
}

type livePrompt struct {
	start, result time.Time
	request       claudeRoundTrip
}

func (c *liveClaude) prompt(t *testing.T) livePrompt {
	t.Helper()
	c.world.model.begin(0)
	index := c.timing.count()
	// Use identical bytes in both arms. The timer includes writing one prompt
	// and ends as soon as this running process emits its native result.
	line := []byte("{\"type\":\"user\",\"message\":{\"role\":\"user\",\"content\":\"Say OK.\"}}\n")
	start := time.Now()
	_, err := c.in.Write(line)
	codexMust(t, err)
	for c.out.Scan() {
		when := time.Now()
		var msg struct{ Type, Subtype, Result string }
		codexMust(t, json.Unmarshal(c.out.Bytes(), &msg))
		if msg.Type == "system" && msg.Subtype == "init" && c.init.IsZero() {
			c.init = when
		}
		if msg.Type == "result" {
			if msg.Subtype != "success" || strings.TrimSpace(msg.Result) != "OK" {
				t.Fatalf("unexpected native result %q: %q", msg.Subtype, msg.Result)
			}
			c.world.model.verify(t)
			return livePrompt{start, when, c.timing.next(t, index)}
		}
	}
	codexMust(t, c.out.Err())
	t.Fatal("Claude exited before emitting a prompt result")
	return livePrompt{}
}

func (c *liveClaude) close(t *testing.T) time.Duration {
	t.Helper()
	start := time.Now()
	codexMust(t, c.in.Close())
	for c.out.Scan() {
	}
	codexMust(t, c.out.Err())
	if err := c.cmd.Wait(); err != nil {
		t.Fatalf("Claude at EOF: %v: %.1000s", err, c.stderr.String())
	}
	elapsed := time.Since(start)
	c.cancel()
	return elapsed
}

type livePairs struct{ off, on, delta []int }

func (pairs *livePairs) add(d [2]time.Duration) {
	pairs.off = append(pairs.off, int(d[0].Microseconds()))
	pairs.on = append(pairs.on, int(d[1].Microseconds()))
	pairs.delta = append(pairs.delta, int((d[1] - d[0]).Microseconds()))
}

func (pairs *livePairs) report(t *testing.T, label string) {
	t.Helper()
	nonpositive := 0
	for _, n := range pairs.delta {
		if n <= 0 {
			nonpositive++
		}
	}
	ms := func(values []int, p int) float64 { return float64(rank(values, p)) / 1000 }
	t.Logf("live vendor=claude boundary=%s pairs=%d off_p50_ms=%.3f off_p95_ms=%.3f on_p50_ms=%.3f on_p95_ms=%.3f delta_p05_ms=%.3f delta_p50_ms=%.3f delta_p95_ms=%.3f nonpositive_pairs=%d", label, len(pairs.delta), ms(pairs.off, 50), ms(pairs.off, 95), ms(pairs.on, 50), ms(pairs.on, 95), ms(pairs.delta, 5), ms(pairs.delta, 50), ms(pairs.delta, 95), nonpositive)
}

func lifecycleHook(t *testing.T, arm int, rows []string, name string, samples map[string][]int) {
	t.Helper()
	found := 0
	for _, row := range rows {
		fields := strings.Fields(row)
		if len(fields) != 4 {
			t.Fatalf("unexpected hook counter %q", row)
		}
		if fields[1] != name {
			continue
		}
		ms, err := strconv.Atoi(fields[3])
		codexMust(t, err)
		samples[name] = append(samples[name], ms)
		found++
	}
	if found != arm {
		t.Fatalf("arm %d: saw %d %s callbacks", arm, found, name)
	}
}

func TestClaudeRunningTurnLatency(t *testing.T) {
	raw := os.Getenv("CROSSHALL_CLAUDE_LIVE_PAIRS")
	if raw == "" {
		t.Skip("set CROSSHALL_CLAUDE_LIVE_PAIRS to opt in")
	}
	count, err := strconv.Atoi(raw)
	if err != nil || count < 1 {
		t.Fatal("CROSSHALL_CLAUDE_LIVE_PAIRS must be positive")
	}
	var spec hook.InstallSpec
	for _, s := range hook.InstallSpecs() {
		if s.Adapter == "claude" {
			spec = s
		}
	}
	if spec.Adapter == "" {
		t.Fatal("no Claude install description")
	}
	var timing [2]claudeLoopbackTiming
	worlds := [2]*measurementWorld{
		makeMeasurementWorld(t, spec, timing[0].observe),
		makeMeasurementWorld(t, spec, timing[1].observe),
	}
	worlds[0].configure(t, "off")
	worlds[1].configure(t, "base")
	var whole, beforeModel, service, afterModel, launch, launchInit, shutdown livePairs
	nativeHook := map[string][]int{}
	completed := 0
	for block := 0; completed < count; block++ {
		var clients [2]*liveClaude
		var startup, init, closeElapsed [2]time.Duration
		var priorMarkers [2]int
		for j := 0; j < 2; j++ {
			arm := (block + j) % 2
			before := len(worlds[arm].counters(t))
			c := openLiveClaude(t, worlds[arm], &timing[arm])
			clients[arm] = c
			// Startup includes the first prompt because print mode does not
			// signal it is ready for user input independently of that prompt.
			first := c.prompt(t)
			lifecycleHook(t, arm, worlds[arm].counters(t)[before:], "SessionStart", nativeHook)
			startup[arm] = first.result.Sub(c.started)
			if c.init.IsZero() {
				t.Fatal("first prompt emitted no native init")
			}
			init[arm] = c.init.Sub(c.started)
			// Consume any enrollment or roster output before timing.
			priorMarkers[arm] = c.prompt(t).request.markers
		}
		launch.add(startup)
		launchInit.add(init)
		for range min(5, count-completed) {
			var elapsed, pre, model, post [2]time.Duration
			for j := 0; j < 2; j++ {
				arm := (completed + j) % 2
				w := worlds[arm]
				before := len(w.counters(t))
				msg := clients[arm].prompt(t)
				if msg.request.markers != priorMarkers[arm] {
					t.Fatalf("arm %d: model-visible crosshall marker count changed from %d to %d in a quiet turn", arm, priorMarkers[arm], msg.request.markers)
				}
				elapsed[arm] = msg.result.Sub(msg.start)
				pre[arm] = msg.request.arrival.Sub(msg.start)
				model[arm] = msg.request.reply.Sub(msg.request.arrival)
				post[arm] = msg.result.Sub(msg.request.reply)
				// The Stop hook appends its counter row after its output, so
				// the row can land just after the native result.
				rows := w.counters(t)[before:]
				for wait := time.Now().Add(2 * time.Second); arm == 1 && len(rows) < 2 && time.Now().Before(wait); rows = w.counters(t)[before:] {
					time.Sleep(time.Millisecond)
				}
				events := map[string]int{}
				for _, row := range rows {
					fields := strings.Fields(row)
					if len(fields) != 4 {
						t.Fatalf("unexpected hook counter %q", row)
					}
					ms, err := strconv.Atoi(fields[3])
					codexMust(t, err)
					events[fields[1]]++
					nativeHook[fields[1]] = append(nativeHook[fields[1]], ms)
				}
				if arm == 0 && len(events) != 0 || arm == 1 && (len(events) != 2 || events["UserPromptSubmit"] != 1 || events["Stop"] != 1) {
					t.Fatalf("arm %d: unexpected quiet-turn callbacks %v", arm, events)
				}
			}
			whole.add(elapsed)
			beforeModel.add(pre)
			service.add(model)
			afterModel.add(post)
			completed++
			t.Logf("quiet_pair vendor=claude n=%d off_us=%d on_us=%d off_pre_us=%d on_pre_us=%d off_post_us=%d on_post_us=%d", completed, elapsed[0].Microseconds(), elapsed[1].Microseconds(), pre[0].Microseconds(), pre[1].Microseconds(), post[0].Microseconds(), post[1].Microseconds())
		}
		for j := 0; j < 2; j++ {
			arm := (block + j) % 2
			before := len(worlds[arm].counters(t))
			closeElapsed[arm] = clients[arm].close(t)
			lifecycleHook(t, arm, worlds[arm].counters(t)[before:], "SessionEnd", nativeHook)
		}
		shutdown.add(closeElapsed)
		t.Logf("lifecycle_pair vendor=claude n=%d off_start_first_us=%d on_start_first_us=%d off_start_init_us=%d on_start_init_us=%d off_close_us=%d on_close_us=%d", block+1, startup[0].Microseconds(), startup[1].Microseconds(), init[0].Microseconds(), init[1].Microseconds(), closeElapsed[0].Microseconds(), closeElapsed[1].Microseconds())
	}
	whole.report(t, "prompt-write-to-final-result")
	beforeModel.report(t, "prompt-write-to-model-request-headers")
	service.report(t, "fixture-handler")
	afterModel.report(t, "fixture-reply-to-final-result")
	launch.report(t, "process-start-to-first-result")
	launchInit.report(t, "process-start-to-native-init")
	shutdown.report(t, "stdin-eof-to-process-exit")
	for _, name := range []string{"UserPromptSubmit", "Stop", "SessionStart", "SessionEnd"} {
		v := nativeHook[name]
		t.Logf("native_hook vendor=claude name=%s starts=%d p50_ms=%d p95_ms=%d", name, len(v), rank(v, 50), rank(v, 95))
	}
	t.Logf("inspection vendor=claude prompts_per_arm=%d requests_per_quiet_turn=1 continuations=0 new_crosshall_markers=0", completed)
}
