// Package telemetrysend freezes and sends the optional daily usage report.
// Only the dispatcher imports it. A report goes out through the system curl,
// so the binary that every hook starts links no TLS code.
package telemetrysend

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/telemetry"
	"github.com/BeomSeogKim/crosshall/internal/telemetryconfig"
)

const (
	// maxAgeDays is the oldest day a batch is frozen or kept for, counted
	// back from today.
	maxAgeDays = 7
	// keepSentDays is how long a sent copy stays for owner telemetry sent.
	keepSentDays = 30
	// sendSpread is how far ahead a new batch's send time is drawn.
	sendSpread = 6 * time.Hour
	firstRetry = time.Hour
	lastRetry  = 24 * time.Hour
	day        = 24 * time.Hour
	// watchEvery is how often a request in flight rereads the setting.
	watchEvery = 200 * time.Millisecond
)

type (
	state    = telemetryconfig.State
	dayState = telemetryconfig.DayState
)

// Config names the files and switches the sender reads.
type Config struct {
	StateRoot  string
	Rendezvous string
	Version    string
	// Endpoint is the receiver's URL. While it is empty the sender does
	// nothing at all.
	Endpoint string
	// PolicyPath is the administrator's opt-out file.
	PolicyPath string
	// Vetoed is true when the dispatcher was launched with an environment
	// opt-out; it holds for the dispatcher's whole life.
	Vetoed bool
	Curl   string
	// caFile, timeout and beforeSend are set only by tests.
	caFile     string
	timeout    time.Duration
	beforeSend func()
}

// Sender is driven by the dispatcher loop: Tick at start and once a minute.
type Sender struct {
	cfg  Config
	db   *store.Store
	busy sync.Mutex
	wg   sync.WaitGroup
}

// New returns a sender for one dispatcher.
func New(cfg Config, db *store.Store) *Sender {
	if cfg.timeout == 0 {
		cfg.timeout = 10 * time.Second
	}
	return &Sender{cfg: cfg, db: db}
}

// Wait returns when no round is running. Cancel the context given to Tick
// first.
func (s *Sender) Wait() { s.wg.Wait() }

// Tick starts one round in the background unless the last one still runs.
// It never blocks the dispatcher loop: building a backlog and a request
// take their time beside it, and the store is held one day at a time.
func (s *Sender) Tick(ctx context.Context, now time.Time) {
	if s.cfg.Endpoint == "" || telemetry.AppVersion(s.cfg.Version) == "dev" || !s.busy.TryLock() {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.busy.Unlock()
		if err := s.round(ctx, now); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("dispatcher: usage report: %v", err)
		}
	}()
}

func (s *Sender) round(ctx context.Context, now time.Time) error {
	var interval time.Time
	var days []time.Time
	err := s.locked(func(dir *os.Root, setting telemetryconfig.Setting, st *state) error {
		if !s.prepare(dir, setting, st, now) {
			return nil
		}
		interval = setting.ChangedAt
		days = toFreeze(st, s.eligibleFrom(setting, st), now)
		return nil
	})
	if err != nil || interval.IsZero() {
		return err
	}
	for _, d := range days {
		if err := s.freeze(ctx, interval, d, now); err != nil {
			return err
		}
	}
	var next *job
	err = s.locked(func(dir *os.Root, setting telemetryconfig.Setting, st *state) error {
		if !s.current(setting, interval) {
			return nil
		}
		var err error
		next, err = due(dir, st, interval, now)
		return err
	})
	if err != nil || next == nil {
		return err
	}
	if s.cfg.beforeSend != nil {
		s.cfg.beforeSend()
	}
	return s.finish(*next, s.deliver(ctx, *next))
}

// locked runs fn under the setting's lock with the state read, and writes
// the state back when fn changed it.
func (s *Sender) locked(fn func(dir *os.Root, setting telemetryconfig.Setting, st *state) error) error {
	return telemetryconfig.Locked(s.cfg.StateRoot, func(dir *os.Root, setting telemetryconfig.Setting) error {
		st, err := telemetryconfig.ReadState(dir)
		if err != nil {
			return err
		}
		before, err := json.Marshal(st)
		if err != nil {
			return err
		}
		err = fn(dir, setting, st)
		if after, _ := json.Marshal(st); !bytes.Equal(before, after) {
			err = errors.Join(err, telemetryconfig.WriteState(dir, st))
		}
		return err
	})
}

// prepare does the bookkeeping every round owes, whatever the setting, and
// reports whether this round may build and send.
func (s *Sender) prepare(dir *os.Root, setting telemetryconfig.Setting, st *state, now time.Time) bool {
	report := func(err error) {
		if err != nil {
			log.Printf("dispatcher: usage report: %v", err)
		}
	}
	report(pruneSent(dir, now))
	if setting.Mode != telemetryconfig.On {
		return false
	}
	if s.suppressed() {
		// Days this dispatcher saw suppressed are never reported.
		st.Suppress(now)
		report(dir.RemoveAll("pending"))
		return false
	}
	if st.StoppedVersion == s.cfg.Version {
		report(dir.RemoveAll("pending"))
		return false
	}
	report(prunePending(dir, st, now))
	report(s.recoverDays(dir, st, setting, now))
	return s.curlPresent()
}

func (s *Sender) curlPresent() bool {
	info, err := os.Stat(s.cfg.Curl)
	return err == nil && info.Mode().IsRegular()
}

func (s *Sender) suppressed() bool {
	return s.cfg.Vetoed || telemetryconfig.ManagedOffAt(s.cfg.PolicyPath)
}

// current reports whether setting is still the consent interval that
// started at interval, with no opt-out in effect.
func (s *Sender) current(setting telemetryconfig.Setting, interval time.Time) bool {
	return setting.Mode == telemetryconfig.On && setting.ChangedAt.Equal(interval) && !s.suppressed()
}

// eligibleFrom is the first day that lies wholly after the setting turned
// on and after the last opt-out a dispatcher saw.
func (s *Sender) eligibleFrom(setting telemetryconfig.Setting, st *state) time.Time {
	from := setting.ChangedAt
	if st.SuppressedThrough.After(from) {
		from = st.SuppressedThrough
	}
	return telemetryconfig.NextFullDay(from)
}

func dayName(t time.Time) string { return t.UTC().Format(time.DateOnly) }

// toFreeze lists the closed eligible days not frozen yet, oldest first.
func toFreeze(st *state, eligible, now time.Time) []time.Time {
	var days []time.Time
	today := now.UTC().Truncate(day)
	for d := today.AddDate(0, 0, -maxAgeDays); d.Before(today); d = d.AddDate(0, 0, 1) {
		if !d.Before(eligible) && st.Days[dayName(d)] == nil {
			days = append(days, d)
		}
	}
	return days
}

// freeze builds day d once: the hook file is counted and the store read in
// a transaction of its own, then the batch is written under the setting's
// lock if the consent interval is still the one it was built for.
func (s *Sender) freeze(ctx context.Context, interval, d, now time.Time) error {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return err
	}
	in := telemetry.Input{Day: d, Batch: hex.EncodeToString(token[:]), Version: s.cfg.Version, Now: now}
	if rv, err := spool.OpenServerRoot(s.cfg.Rendezvous); err == nil {
		raw, ok := telemetry.ReadHookFile(rv, d)
		rv.Close()
		if ok {
			in.Hooks, in.HooksRead = telemetry.CountHooks(raw), true
		}
	}
	var r telemetry.Report
	if err := s.db.Update(ctx, func(tx store.Tx) error {
		var err error
		r, err = telemetry.Build(ctx, tx, in)
		return err
	}); err != nil {
		return err
	}
	spread, err := rand.Int(rand.Reader, big.NewInt(int64(sendSpread)))
	if err != nil {
		return err
	}
	return s.locked(func(dir *os.Root, setting telemetryconfig.Setting, st *state) error {
		name := dayName(d)
		if !s.current(setting, interval) || st.Days[name] != nil {
			return nil
		}
		if !r.Active() {
			st.Days[name] = &dayState{}
			return nil
		}
		// The week figures count only days of this consent interval.
		eligible := s.eligibleFrom(setting, st)
		active, unobserved := int64(1), r.Unobserved10m
		for back := 1; back < 7; back++ {
			prior := d.AddDate(0, 0, -back)
			if p := st.Days[dayName(prior)]; p != nil && p.Batch && !prior.Before(eligible) {
				active, unobserved = active+1, unobserved+p.Unobserved
			}
		}
		r.SetWeek(active, unobserved)
		body, err := r.Marshal()
		if err == nil {
			err = telemetry.Validate(body, now)
		}
		if err == nil && len(body) > telemetryconfig.MaxReport {
			err = fmt.Errorf("report is %d bytes", len(body))
		}
		if err != nil {
			// Never retried: the day stays recorded without a batch.
			log.Printf("dispatcher: usage report for %s not frozen: %v", name, err)
			st.Days[name] = &dayState{}
			return nil
		}
		// The batch is written before the state; a restart in between
		// adopts these bytes instead of building the day again.
		if err := telemetryconfig.WriteDayFile(dir, "pending", name, body); err != nil {
			return err
		}
		st.Days[name] = &dayState{Batch: true, Unobserved: r.Unobserved10m, NotBefore: now.Add(time.Duration(spread.Int64()))}
		return nil
	})
}

// recoverDays adopts batches the state does not know, as after a restart
// between writing a batch and the state, and finishes the archive move of
// batches the receiver accepted.
func (s *Sender) recoverDays(dir *os.Root, st *state, setting telemetryconfig.Setting, now time.Time) error {
	pending, err := telemetryconfig.ListDays(dir, "pending")
	if err != nil {
		return err
	}
	eligible := s.eligibleFrom(setting, st)
	for _, name := range pending {
		info := st.Days[name]
		if info != nil && info.Accepted {
			if err := archive(dir, name); err != nil {
				return err
			}
			continue
		}
		if info != nil {
			continue
		}
		body, err := telemetryconfig.ReadDayFile(dir, "pending", name)
		var r telemetry.Report
		if err == nil {
			r, err = telemetry.Parse(body, now)
		}
		if d, _ := time.Parse(time.DateOnly, name); err != nil || d.Before(eligible) {
			if err := dir.Remove(filepath.Join("pending", name+".json")); err != nil {
				return err
			}
			continue
		}
		st.Days[name] = &dayState{Batch: true, Unobserved: r.Counts["notice.unobserved_10m"], NotBefore: now}
	}
	sent, err := telemetryconfig.ListDays(dir, "sent")
	if err != nil {
		return err
	}
	for _, name := range sent {
		if d, _ := time.Parse(time.DateOnly, name); st.Days[name] == nil && !d.Before(eligible) {
			st.Days[name] = &dayState{Batch: true, Accepted: true}
		}
	}
	return nil
}

type job struct {
	day      string
	body     []byte
	interval time.Time
	now      time.Time
}

// due returns the oldest pending batch when its time has come. A first
// attempt needs PostGap since the last one; a retry of the same batch does
// not.
func due(dir *os.Root, st *state, interval, now time.Time) (*job, error) {
	days, err := telemetryconfig.ListDays(dir, "pending")
	if err != nil {
		return nil, err
	}
	for _, name := range days {
		info := st.Days[name]
		if info == nil || info.Accepted {
			continue
		}
		if now.Before(st.NextAttempt(name)) {
			return nil, nil
		}
		body, err := telemetryconfig.ReadDayFile(dir, "pending", name)
		if err != nil {
			return nil, err
		}
		if info.Attempts == 0 {
			st.LastPost = now
		}
		info.Attempts++
		return &job{day: name, body: body, interval: interval, now: now}, nil
	}
	return nil, nil
}

// result is what one request ended with.
type result struct {
	status     int // 0: no HTTP response
	retryAfter time.Duration
	// cancelled: the request was stopped or never started; suppressedAt
	// is set when an opt-out was the reason.
	cancelled    bool
	suppressedAt time.Time
}

// stillWanted reports whether j may be sent now: the same consent interval,
// no opt-out, and its batch still pending.
func (s *Sender) stillWanted(j job) (bool, bool) {
	if s.suppressed() {
		return false, true
	}
	setting, err := telemetryconfig.ReadMode(s.cfg.StateRoot)
	if err != nil || !s.current(setting, j.interval) {
		return false, false
	}
	_, err = os.Lstat(filepath.Join(s.cfg.StateRoot, "telemetry", "pending", j.day+".json"))
	return err == nil, false
}

// deliver runs one POST through curl. It is cancelled as soon as the batch
// stops being wanted.
func (s *Sender) deliver(ctx context.Context, j job) result {
	start := time.Now()
	stopped := func(suppressed bool) result {
		res := result{cancelled: true}
		if suppressed {
			res.suppressedAt = j.now.Add(time.Since(start))
		}
		return res
	}
	if ok, suppressed := s.stillWanted(j); !ok {
		return stopped(suppressed)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	unwanted := make(chan bool, 1)
	go func() {
		ticker := time.NewTicker(watchEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if ok, suppressed := s.stillWanted(j); !ok {
					unwanted <- suppressed
					cancel()
					return
				}
			}
		}
	}()
	args := []string{"-q", "--silent", "--show-error", "--proto", "=https", "--noproxy", "*",
		"--max-time", strconv.FormatFloat(s.cfg.timeout.Seconds(), 'f', -1, 64), "--request", "POST",
		"--header", "Content-Type: application/json", "--header", "Expect:",
		"--user-agent", "crosshall/" + s.cfg.Version,
		"--data-binary", "@-", "--output", "/dev/null", "--dump-header", "-"}
	if s.cfg.caFile != "" {
		args = append(args, "--cacert", s.cfg.caFile)
	}
	cmd := exec.CommandContext(ctx, s.cfg.Curl, append(args, "--url", s.cfg.Endpoint)...)
	cmd.Env, cmd.Dir = []string{}, "/"
	cmd.Stdin = bytes.NewReader(j.body)
	headers := &limitedWriter{limit: 16 << 10}
	cmd.Stdout = headers
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	select {
	case suppressed := <-unwanted:
		return stopped(suppressed)
	default:
	}
	if ctx.Err() != nil {
		return result{cancelled: true}
	}
	if err != nil {
		return result{}
	}
	return parseHeaders(headers.Bytes(), time.Now())
}

// finish records a request's outcome, if the batch is still wanted.
func (s *Sender) finish(j job, res result) error {
	return s.locked(func(dir *os.Root, setting telemetryconfig.Setting, st *state) error {
		if !res.suppressedAt.IsZero() || s.suppressed() {
			// An opt-out seen during the request outlives it.
			st.Suppress(j.now)
			st.Suppress(res.suppressedAt)
			return dir.RemoveAll("pending")
		}
		info := st.Days[j.day]
		if res.cancelled || !s.current(setting, j.interval) || info == nil {
			return nil
		}
		if _, err := dir.Lstat(filepath.Join("pending", j.day+".json")); err != nil {
			return nil
		}
		return apply(dir, st, info, j, res, s.cfg.Version)
	})
}

// apply carries out the response table.
func apply(dir *os.Root, st *state, info *dayState, j job, res result, version string) error {
	switch {
	case res.status == 204:
		st.LastStatus, info.Accepted = "204", true
		// The acceptance is in the state even when the move fails; a
		// later round finishes the move without a second request.
		if err := telemetryconfig.WriteState(dir, st); err != nil {
			return err
		}
		return archive(dir, j.day)
	case slices.Contains([]int{400, 413, 415, 422}, res.status):
		st.LastStatus = strconv.Itoa(res.status)
		return dir.Remove(filepath.Join("pending", j.day+".json"))
	case res.status == 410:
		st.LastStatus, st.StoppedVersion = "410", version
		return dir.RemoveAll("pending")
	}
	st.LastStatus = "no answer"
	if res.status != 0 {
		st.LastStatus = strconv.Itoa(res.status)
	}
	wait := firstRetry << min(info.Attempts-1, 5)
	if res.retryAfter > 0 {
		wait = res.retryAfter
	}
	info.NotBefore = j.now.Add(min(wait, lastRetry))
	return nil
}

// archive moves an accepted batch from pending/ to sent/.
func archive(dir *os.Root, name string) error {
	body, err := telemetryconfig.ReadDayFile(dir, "pending", name)
	if err != nil {
		return err
	}
	if err := telemetryconfig.WriteDayFile(dir, "sent", name, body); err != nil {
		return err
	}
	return dir.Remove(filepath.Join("pending", name+".json"))
}

// parseHeaders reads the status of the last response and its Retry-After,
// honoured up to a day.
func parseHeaders(raw []byte, now time.Time) result {
	var res result
	for line := range strings.Lines(string(raw)) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "HTTP/") {
			fields := strings.Fields(line)
			res = result{}
			if len(fields) >= 2 {
				res.status, _ = strconv.Atoi(fields[1])
			}
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "Retry-After") {
			continue
		}
		value = strings.TrimSpace(value)
		if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
			res.retryAfter = time.Duration(min(seconds, int(lastRetry/time.Second))) * time.Second
		} else if at, err := time.Parse(time.RFC1123, value); err == nil && at.After(now) {
			res.retryAfter = min(at.Sub(now), lastRetry)
		}
	}
	return res
}

// limitedWriter keeps the first limit bytes written to it. It has no
// ReadFrom, so a stream copy goes through Write and its cap.
type limitedWriter struct {
	buf   []byte
	limit int
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	if room := w.limit - len(w.buf); room > 0 {
		w.buf = append(w.buf, p[:min(len(p), room)]...)
	}
	return len(p), nil
}

func (w *limitedWriter) Bytes() []byte { return w.buf }

// prunePending deletes batches older than maxAgeDays unsent and forgets
// days older than the week figures need.
func prunePending(dir *os.Root, st *state, now time.Time) error {
	today := now.UTC().Truncate(day)
	days, err := telemetryconfig.ListDays(dir, "pending")
	if err != nil {
		return err
	}
	for _, name := range days {
		if name < dayName(today.AddDate(0, 0, -maxAgeDays)) {
			if err := dir.Remove(filepath.Join("pending", name+".json")); err != nil {
				return err
			}
		}
	}
	for name := range st.Days {
		if name < dayName(today.AddDate(0, 0, -maxAgeDays-6)) {
			delete(st.Days, name)
		}
	}
	return nil
}

// pruneSent deletes sent copies after keepSentDays, in every state.
func pruneSent(dir *os.Root, now time.Time) error {
	sent, err := telemetryconfig.ListDays(dir, "sent")
	if err != nil {
		return err
	}
	oldest := dayName(now.UTC().Truncate(day).AddDate(0, 0, -keepSentDays))
	for _, name := range sent {
		if name < oldest {
			if err := dir.Remove(filepath.Join("sent", name+".json")); err != nil {
				return err
			}
		}
	}
	return nil
}
