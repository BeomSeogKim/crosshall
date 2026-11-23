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
	"github.com/BeomSeogKim/crosshall/internal/telemetryconfig"
)

// TestRandomInterleavings runs seeded random sequences of rounds, restarts,
// clock jumps across midnight, activity, answers, failed replacements of
// state.json, and opt-outs that appear and disappear at every check point
// (before every step, which is where the sender reads the opt-outs) and
// during requests. Its oracle is computed here, not by the sender: an
// opt-out in effect at a check point was observed on the day of that
// instant, and an on starts consent on its day. No request may carry a day
// up to the latest observed opt-out day, up to the day of the last on, or
// older than seven days, or start while the setting is off, judged when the
// last check before it passed.
//
// A restart forgets what the old sender held in memory. When state.json
// could not be written during the old sender's life, the oracle falls back
// to the opt-out day on disk; otherwise the day on disk must already be the
// oracle's.
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
	var posts, vetoed, lost atomic.Int64
	t.Cleanup(func() {
		t.Logf("%d cases made %d requests; %d cases observed an opt-out; %d restarts lost an undurable opt-out day", cases, posts.Load(), vetoed.Load(), lost.Load())
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
			lost.Add(int64(in.lost))
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
	// denied makes replacing state.json fail; deniedInLife is set while
	// the current sender has seen a denial.
	denied, deniedInLife bool
	lost                 int
	// atStart is the world when the last check before the running
	// request passed; an opt-out after it is the request's own race,
	// which the watcher ends.
	atStart struct {
		mode, onDay, vetoDay string
		clock                time.Time
	}
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
		failStateWrite: in.writeDenied, requestStarts: in.requestStarts,
	}
	return in
}

func (in *interleaving) now() time.Time {
	in.mu.Lock()
	defer in.mu.Unlock()
	return in.clock
}

func (in *interleaving) chance(p float64) bool { return in.rng.Float64() < p }

func (in *interleaving) requestStarts() {
	in.mu.Lock()
	defer in.mu.Unlock()
	in.atStart.mode, in.atStart.onDay, in.atStart.vetoDay, in.atStart.clock = in.mode, in.onDay, in.vetoDay, in.clock
}

func (in *interleaving) writeDenied() bool {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.denied {
		in.deniedInLife = true
	}
	return in.denied
}

// mutate changes the world at random; the caller holds mu.
func (in *interleaving) mutate() {
	switch n := in.rng.IntN(12); {
	case n < 2:
		in.denied = !in.denied
	case n < 5:
		in.setAdmin(!in.admin)
	case n < 7:
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
	if w := in.atStart; w.mode != "on" || report.Day <= w.vetoDay || report.Day <= w.onDay || report.Day < telemetryconfig.OldestDay(w.clock) {
		in.t.Errorf("sent %s at %s with mode %q, on since %s, latest opt-out day %s", report.Day, w.clock.Format(time.RFC3339Nano), w.mode, w.onDay, w.vetoDay)
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

// restarted adjusts the oracle for a sender that forgets its memory; the
// caller holds mu.
func (in *interleaving) restarted() {
	var st struct {
		VetoDay string `json:"veto_day"`
	}
	raw, err := os.ReadFile(filepath.Join(in.state, "telemetry", "state.json"))
	if err == nil {
		err = json.Unmarshal(raw, &st)
	}
	in.must(err)
	switch {
	case st.VetoDay >= in.vetoDay:
	case in.deniedInLife:
		in.vetoDay = st.VetoDay
		in.lost++
	default:
		in.t.Errorf("opt-out day %s on disk at a restart, observed %s, and no write failed", st.VetoDay, in.vetoDay)
	}
	in.deniedInLife = in.denied
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
		// Opt-outs and faults come and go.
		if in.denied && in.chance(0.4) {
			in.denied = false
		}
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
			in.restarted()
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
