//go:build conformance

package hook

import (
	"crypto/sha256"
	"fmt"
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/proc"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// The prompt-side cost of a late start: once per launch the record write
// (ancestor walk, lock, file), and on every prompt the PID namespace read;
// ack_read is the acknowledgement read a turn end adds.
// Diagnostic only; the hook's own budget is 28 ms at p95.
func TestLateStartHookCost(t *testing.T) {
	leaf := hookFileRoot(t)
	hookFile(t, leaf, "gen", "g1\n")
	pid := os.Getppid() // the hook's own parent, so the ancestor walk succeeds at once
	e := event{Identity: ident.Identity{Vendor: "claude"}, LaunchRef: strconv.Itoa(pid), Fact: wire.HookPromptSubmitted,
		EventRef: "turn:UserPromptSubmit", name: "UserPromptSubmit"}
	spec := eventSpec{Name: "UserPromptSubmit", Fact: wire.HookPromptSubmitted}
	name := fmt.Sprintf("launch-%d-%x", pid, sha256.Sum256([]byte("g1")))
	const samples = 300
	metrics := map[string][]time.Duration{}
	timed := func(metric string, fn func()) {
		start := time.Now()
		fn()
		metrics[metric] = append(metrics[metric], time.Since(start))
	}
	for i := range samples {
		_ = leaf.Remove(name)
		_ = leaf.Remove(name + ".lock")
		at := time.UnixMilli(1_000 + int64(i))
		timed("first_prompt_writes_record", func() {
			if b, err := bindLaunch(leaf, "g1", e, spec, at); err != nil || !b.late {
				t.Fatalf("late write: %+v, %v", b, err)
			}
		})
		timed("later_prompt_reads_record", func() {
			if b, err := bindLaunch(leaf, "g1", e, spec, at.Add(time.Millisecond)); err != nil || !b.verified || b.late {
				t.Fatalf("read: %+v, %v", b, err)
			}
		})
		bound, _ := bindLaunch(leaf, "g1", e, spec, at.Add(time.Millisecond))
		ackLaunch(leaf, bound)
		timed("ack_read", func() {
			if !launchAcked(leaf, bound) {
				t.Fatal("acknowledgement not read")
			}
		})
		timed("pid_namespace", func() { _ = proc.PIDNamespace() })
		timed("hosted_by", func() {
			if !proc.HostedBy(pid) {
				t.Fatal("not hosted by the parent")
			}
		})
	}
	for _, metric := range []string{"first_prompt_writes_record", "later_prompt_reads_record", "ack_read", "hosted_by", "pid_namespace"} {
		times := metrics[metric]
		slices.Sort(times)
		t.Logf("%s n=%d p50_us=%.1f p95_us=%.1f max_us=%.1f", metric, samples,
			float64(times[samples/2-1])/1e3, float64(times[samples*95/100-1])/1e3, float64(times[samples-1])/1e3)
	}
}
