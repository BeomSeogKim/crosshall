package telemetrysend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// TestRandomInterleavings runs seeded random sequences of rounds, restarts,
// clock jumps across midnight, activity, answers, and opt-outs that appear
// and disappear at every check point (before every step, which is where the
// sender reads the opt-outs) and during requests. Its oracle is computed
// here, not by the sender: an opt-out in effect at a check point was
// observed on the day of that instant, and an on starts consent on its
// day. No request may carry a day up to the latest observed opt-out day or
// up to the day of the last on, or start while the setting is off.
//
// It runs 1000 cases by default; CROSSHALL_TELEMETRY_CASES sets another
// number, as for a longer run under the race detector.
func TestRandomInterleavings(t *testing.T) {
	cases := 1000
	if n, err := strconv.Atoi(os.Getenv("CROSSHALL_TELEMETRY_CASES")); err == nil {
		cases = n
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	var posts, vetoed atomic.Int64
	t.Cleanup(func() {
		t.Logf("%d cases made %d requests; %d cases observed an opt-out", cases, posts.Load(), vetoed.Load())
		if posts.Load() < int64(cases) {
			t.Errorf("only %d requests in %d cases", posts.Load(), cases)
		}
	})
	for seed := range uint64(cases) {
		t.Run(fmt.Sprint(seed), func(t *testing.T) {
			t.Parallel()
			in := newInterleaving(t, db, seed)
			in.run()
			posts.Add(int64(in.posts))
			if in.vetoDay != "" {
				vetoed.Add(1)
			}
		})
	}
}

type interleaving struct {
	t     *testing.T
	db    *store.Store
	state string
	rv    string
	cfg   Config

	// step is held from a check point to the end of its step, so the world
	// the oracle saw at the check point is the world the sender reads.
	step    sync.Mutex
	mu      sync.Mutex
	rng     *rand.Rand
	clock   time.Time
	mode    string // "", "on", "off"
	onDay   string
	admin   bool
	vetoed  bool
	vetoDay string // latest day an opt-out was in effect at a check point
	posts   int
}

func newInterleaving(t *testing.T, db *store.Store, seed uint64) *interleaving {
	dir := t.TempDir()
	in := &interleaving{
		t: t, db: db, state: filepath.Join(dir, "state"), rv: filepath.Join(dir, "rv"),
		rng: rand.New(rand.NewPCG(seed, 0x7e1e)), clock: d0.Add(time.Duration(seed%86400) * time.Second),
	}
	for _, path := range []string{in.state, in.rv} {
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	}
	in.cfg = Config{
		StateRoot: in.state, Rendezvous: in.rv, Version: "v0.2.0-rc.2", Endpoint: "https://receiver.invalid/v1/report",
		PolicyPath: filepath.Join(dir, "telemetry-off"), watchEvery: time.Millisecond,
		clock: in.now, checkpoint: in.checkpoint, stepDone: in.step.Unlock, post: in.post,
	}
	return in
}

func (in *interleaving) now() time.Time {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.clock
}

func (in *interleaving) chance(p float64) bool { return in.rng.Float64() < p }

// mutate changes the world at random; the caller holds mu.
func (in *interleaving) mutate() {
	switch n := in.rng.IntN(10); {
	case n < 3:
		in.setAdmin(!in.admin)
	case n < 5:
		if in.mode == "on" {
			in.setMode("off")
		} else {
			in.setMode("on")
		}
	default:
		// Small steps, which cross midnight when the clock is close to it.
		in.clock = in.clock.Add(time.Duration(in.rng.IntN(1500)) * time.Millisecond)
	}
}

// checkpoint runs before every step: the world may change, and whatever
// opt-out is then in effect is observed at this instant.
func (in *interleaving) checkpoint() {
	in.step.Lock()
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.chance(0.05) {
		in.mutate()
	}
	if in.admin || in.vetoed || in.mode == "off" {
		if day := dayName(in.clock); day > in.vetoDay {
			in.vetoDay = day
		}
	}
}

func (in *interleaving) post(ctx context.Context, body []byte) result {
	var report struct{ Day string }
	if err := json.Unmarshal(body, &report); err != nil {
		in.t.Errorf("unreadable body %s", body)
	}
	in.step.Lock()
	defer in.step.Unlock()
	in.mu.Lock()
	in.posts++
	if in.mode != "on" || report.Day <= in.vetoDay || report.Day <= in.onDay {
		in.t.Errorf("sent %s with mode %q, on since %s, latest opt-out day %s", report.Day, in.mode, in.onDay, in.vetoDay)
	}
	during := in.chance(0.1)
	if during {
		in.mutate()
	}
	var res result
	switch n := in.rng.IntN(10); {
	case n < 6:
		res.status = 204
	case n == 6:
		res.status = 500
	case n == 7:
		res.status, res.retryAfter = 429, time.Duration(in.rng.IntN(7200))*time.Second
	case n == 8:
		res.status = 400
	default:
		res.status = []int{0, 410}[in.rng.IntN(2)]
	}
	in.mu.Unlock()
	if during {
		// Give the watcher a chance to see the change.
		select {
		case <-ctx.Done():
		case <-time.After(5 * time.Millisecond):
		}
	}
	return res
}

func (in *interleaving) setAdmin(on bool) {
	in.admin = on
	if on {
		in.must(os.WriteFile(in.cfg.PolicyPath, nil, 0644))
	} else {
		in.must(os.Remove(in.cfg.PolicyPath))
	}
}

// setMode writes telemetry/mode as the CLI does, without its cleanup, as if
// that cleanup had failed.
func (in *interleaving) setMode(mode string) {
	dir := filepath.Join(in.state, "telemetry")
	in.must(os.MkdirAll(dir, 0700))
	line := mode + " " + in.clock.Format(time.RFC3339Nano) + " owner-command\n"
	// Replaced atomically, as the CLI replaces it.
	stage := filepath.Join(dir, "mode.test-part")
	in.must(os.WriteFile(stage, []byte(line), 0600))
	in.must(os.Rename(stage, filepath.Join(dir, "mode")))
	in.mode = mode
	if mode == "on" {
		in.onDay = dayName(in.clock)
	}
}

func (in *interleaving) activity() {
	rv, err := spool.OpenServerRoot(in.rv)
	in.must(err)
	defer rv.Close()
	dir, err := spool.OpenServerChild(rv, "hooks", true)
	in.must(err)
	defer dir.Close()
	f, err := spool.OpenServerFile(dir, in.clock.UTC().Format("20060102"), os.O_WRONLY|os.O_CREATE|os.O_APPEND)
	in.must(err)
	defer f.Close()
	_, err = io.WriteString(f, "claude UserPromptSubmit clean 3\n")
	in.must(err)
}

func (in *interleaving) must(err error) {
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		in.t.Fatal(err)
	}
}

func (in *interleaving) run() {
	in.mu.Lock()
	in.setMode("on")
	in.mu.Unlock()
	// The world changes only at a check point, inside a request, or here
	// between rounds; each holds step.
	s := New(in.cfg, in.db)
	for range 80 {
		in.step.Lock()
		in.mu.Lock()
		// Opt-outs come and go.
		if in.admin && in.chance(0.4) {
			in.setAdmin(false)
		}
		if in.mode == "off" && in.chance(0.4) {
			in.setMode("on")
		}
		action := in.rng.IntN(12)
		switch {
		case action < 3:
			in.clock = in.clock.Add(time.Duration(in.rng.IntN(30*60)) * time.Minute)
		case action == 3:
			// Just before a midnight.
			next := in.clock.UTC().Truncate(day).Add(day)
			in.clock = next.Add(-time.Duration(in.rng.IntN(3000)) * time.Millisecond)
		case action < 6:
			in.activity()
		case action == 6 && in.chance(0.3):
			in.mutate()
		case action == 7:
			// A restart, perhaps with an environment veto or a new release.
			in.vetoed = in.chance(0.1)
			cfg := in.cfg
			cfg.Vetoed = in.vetoed
			if in.chance(0.2) {
				cfg.Version = "v0.2.0-rc.3"
			}
			s = New(cfg, in.db)
		}
		now := in.clock
		in.mu.Unlock()
		in.step.Unlock()
		if action >= 8 {
			s.Tick(in.t.Context(), now)
			s.Wait()
		}
	}
}
