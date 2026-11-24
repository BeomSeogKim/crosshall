//go:build conformance

package hook

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// A diagnostic control for a latency miss. These isolated phase samples do
// not change the product hook or add timers to a measured vendor turn.
func TestQuietHookCostBreakdown(t *testing.T) {
	bin := os.Getenv("CROSSHALL_PHASE_BINARY")
	if bin == "" {
		t.Skip("set CROSSHALL_PHASE_BINARY to a development build")
	}
	if !filepath.IsAbs(bin) {
		t.Fatal("expected an absolute crosshall path")
	}
	rv, dir, member := runFixture(t)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for file, value := range map[string]string{"stamp": "fixture-host fixture-gen 1 0\n", "seen-notice": "fixture-host fixture-gen 1\n"} {
		must(os.WriteFile(filepath.Join(member, file), []byte(value), 0600))
	}
	must(os.Mkdir(filepath.Join(member, "in"), 0700))
	leaf, err := os.OpenRoot(member)
	must(err)
	defer leaf.Close()
	const samples = 200
	metrics := map[string][]time.Duration{}
	timed := func(name string, fn func()) {
		start := time.Now()
		fn()
		metrics[name] = append(metrics[name], time.Since(start))
	}
	identity := ident.Identity{Vendor: "codex", Key: "fixture-thread"}
	client := spool.Client{Rendezvous: dir}
	input := []byte(`{"hook_event_name":"UserPromptSubmit","session_id":"fixture-thread","turn_id":"fixture-turn"}`)
	for range samples {
		// This enters the actual Go binary and exits before resolving paths.
		cmd := exec.Command(bin, "hook")
		cmd.Dir, cmd.Env = dir, []string{"HOME=" + dir, "PATH=/usr/bin:/bin", "CROSSHALL_STATE_DIR=" + dir}
		timed("process_start_to_noop_exit", func() { must(cmd.Run()) })
		timed("read_policy", func() { _, err := readPolicy(rv, "codex"); must(err) })
		timed("clean_stamp_and_seen", func() {
			if !stampClean(rv, leaf, "fixture-gen", "notice", "") {
				t.Fatal("expected clean stamp")
			}
		})
		var counter *hookCounter
		timed("open_counter", func() {
			counter = openHookCounter(rv, "codex", "UserPromptSubmit", time.Now())
			if counter == nil {
				t.Fatal("missing private counter")
			}
		})
		timed("append_and_close_counter", func() { counter.append("clean", time.Now()); must(counter.file.Close()) })
		timed("publish_one_way", func() {
			ctx, cancel := context.WithTimeout(t.Context(), Budget)
			defer cancel()
			_, err := client.HookCall(ctx, identity, "fixture-gen", wire.Request{Verb: wire.VerbHookEvent, OpKey: "hook:codex:session:fixture-turn:UserPromptSubmit"}, true)
			must(err)
		})
		timed("full_in_process_prompt", func() {
			// Match the host's payload-only identity regardless of the
			// ambient agent used to start this benchmark.
			runTimed(time.Now(), Budget, io.Discard, func() (*os.Root, *hookCounter) {
				r, err := ident.OpenPrivateDir(dir)
				if err != nil {
					return nil, nil
				}
				return r, openHookCounter(r, "codex", "UserPromptSubmit", time.Now())
			}, func(ctx context.Context, root *os.Root, out io.Writer) string {
				return dispatchEvent(ctx, time.Now(), root, client, []string{"codex", "UserPromptSubmit"}, bytes.NewReader(input), out, func(string) (string, bool) { return "", false })
			})
		})
	}
	// Both the isolated writes and the real clean callback must have
	// appended successfully. A refusal would otherwise look deceptively fast.
	files, err := filepath.Glob(filepath.Join(dir, "hooks", "????????"))
	must(err)
	var clean int
	for _, file := range files {
		data, err := os.ReadFile(file)
		must(err)
		clean += strings.Count(string(data), "codex UserPromptSubmit clean ")
	}
	if clean != samples*2 {
		t.Fatalf("recorded %d successful operations, want %d", clean, 2*samples)
	}
	for name, times := range metrics {
		slices.Sort(times)
		t.Logf("%s n=%d p50_us=%.3f p95_us=%.3f", name, samples, float64(times[samples/2-1])/1e3, float64(times[samples*95/100-1])/1e3)
	}
}
