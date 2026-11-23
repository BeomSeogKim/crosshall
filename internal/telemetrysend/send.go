// Package telemetrysend freezes and sends the optional daily usage report.
// Only the dispatcher imports it. A report goes out through the system curl,
// so the binary that every hook starts links no TLS code.
//
// Consent rests on two rules. observe is the only code that reads the
// opt-outs (the owner's off, an environment veto, the administrator's file);
// whenever it sees one it records that UTC day before it returns, and that
// day and every earlier one are never sent. telemetryconfig.Cutoff is the
// only cutoff: nothing is built, selected or handed to curl for a day up to
// it.
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
	maxAgeDays = telemetryconfig.MaxAgeDays
	// keepSentDays is how long the bytes of an accepted report stay,
	// counted from the acceptance.
	keepSentDays = 30
	// sendSpread is how far ahead a new batch's send time is drawn.
	sendSpread = 6 * time.Hour
	firstRetry = time.Hour
	lastRetry  = 24 * time.Hour
	day        = 24 * time.Hour
)

type (
	state    = telemetryconfig.State
	dayState = telemetryconfig.DayState
	setting  = telemetryconfig.Setting
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

	// Set only by tests.
	caFile     string
	timeout    time.Duration
	watchEvery time.Duration
	// clock replaces the dispatcher's clock; checkpoint and stepDone run
	// around every step, where the opt-outs are read; post replaces curl;
	// failStateWrite makes replacing state.json fail; requestStarts runs
	// when the last check before a request passed.
	failStateWrite func() bool
	requestStarts  func()
	clock          func() time.Time
	checkpoint     func()
	stepDone       func()
	post           func(ctx context.Context, body []byte) result
	beforeSend     func()
	afterPrepare   func()
}

// Sender is driven by the dispatcher loop: Tick at start and once a minute.
type Sender struct {
	cfg  Config
	db   *store.Store
	busy sync.Mutex
	wg   sync.WaitGroup
	// base and started give the running round's clock.
	base, started time.Time
	// vetoDay is the latest opt-out day this sender observed or read. It
	// survives a failed state write; until state.json holds it, nothing is
	// built or sent.
	vetoMu  sync.Mutex
	vetoDay string
}

// remember raises the opt-out day held in memory and returns it.
func (s *Sender) remember(day string) string {
	s.vetoMu.Lock()
	defer s.vetoMu.Unlock()
	if day > s.vetoDay {
		s.vetoDay = day
	}
	return s.vetoDay
}

func (s *Sender) writeState(dir *os.Root, st *state) error {
	if s.cfg.failStateWrite != nil && s.cfg.failStateWrite() {
		return errors.New("telemetry/state.json: replacement failed (test)")
	}
	return telemetryconfig.WriteState(dir, st)
}

// New returns a sender for one dispatcher.
func New(cfg Config, db *store.Store) *Sender {
	if cfg.timeout == 0 {
		cfg.timeout = 10 * time.Second
	}
	if cfg.watchEvery == 0 {
		cfg.watchEvery = 200 * time.Millisecond
	}
	return &Sender{cfg: cfg, db: db}
}

// at is the dispatcher's time now.
func (s *Sender) at() time.Time {
	if s.cfg.clock != nil {
		return s.cfg.clock()
	}
	return s.base.Add(time.Since(s.started))
}

// Wait returns when no round is running. Cancel the context given to Tick
// first.
func (s *Sender) Wait() { s.wg.Wait() }

// Tick starts one round in the background unless the last one still runs.
// It never blocks the dispatcher loop: building a backlog and a request
// take their time beside it, and the store is read on its own connection.
func (s *Sender) Tick(ctx context.Context, now time.Time) {
	if s.cfg.Endpoint == "" || telemetry.AppVersion(s.cfg.Version) == "dev" || !s.busy.TryLock() {
		return
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer s.busy.Unlock()
		s.base, s.started = now, time.Now()
		if err := s.round(ctx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("dispatcher: usage report: %v", err)
		}
	}()
}

// observe is the one place that reads the opt-outs. When one is in effect
// it raises the opt-out day in memory to the UTC day of this instant,
// rounded up to the whole day, and drops the batches not yet accepted. The
// step that called it persists the day before anything else happens.
func (s *Sender) observe(dir *os.Root, set setting, st *state) (bool, error) {
	at := s.at()
	vetoed := s.cfg.Vetoed || telemetryconfig.ManagedOffAt(s.cfg.PolicyPath)
	if !vetoed && set.Mode != telemetryconfig.Off {
		return false, nil
	}
	s.remember(dayName(at))
	return true, telemetryconfig.DropPending(dir, st)
}

// step runs fn under the setting's lock with the state read, only when
// observe found no opt-out, the opt-out day in memory is in state.json and
// the setting is on, and writes the state back when it changed. Every
// decision of a round is taken in a step. A failed write of the opt-out day
// stops the step (fail closed); the next step tries again.
func (s *Sender) step(fn func(dir *os.Root, set setting, st *state) error) error {
	if s.cfg.checkpoint != nil {
		s.cfg.checkpoint()
	}
	if s.cfg.stepDone != nil {
		defer s.cfg.stepDone()
	}
	return telemetryconfig.Locked(s.cfg.StateRoot, func(dir *os.Root, set setting) error {
		st, err := telemetryconfig.ReadState(dir)
		if err != nil {
			return err
		}
		before, err := json.Marshal(st)
		if err != nil {
			return err
		}
		if err := housekeeping(dir, st, s.at()); err != nil {
			log.Printf("dispatcher: usage report: %v", err)
		}
		durable := st.VetoDay
		s.remember(durable)
		off, err := s.observe(dir, set, st)
		if day := s.remember(""); day > durable {
			st.VetoDay = day
			if werr := s.writeState(dir, st); werr != nil {
				return errors.Join(err, werr)
			}
			before, _ = json.Marshal(st)
		}
		switch {
		case off || err != nil || set.Mode != telemetryconfig.On:
		case st.StoppedVersion == s.cfg.Version:
			err = telemetryconfig.DropPending(dir, st)
		default:
			err = fn(dir, set, st)
		}
		if after, _ := json.Marshal(st); !bytes.Equal(before, after) {
			err = errors.Join(err, s.writeState(dir, st))
		}
		return err
	})
}

func (s *Sender) round(ctx context.Context) error {
	now := s.at()
	var days []time.Time
	proceed := false
	if err := s.step(func(dir *os.Root, set setting, st *state) error {
		cutoff := telemetryconfig.Cutoff(set, st)
		if err := recoverDays(dir, st, cutoff, now); err != nil {
			return err
		}
		proceed = s.curlPresent()
		days = toFreeze(st, cutoff, now)
		return nil
	}); err != nil || !proceed {
		return err
	}
	if s.cfg.afterPrepare != nil {
		s.cfg.afterPrepare()
	}
	for _, d := range days {
		if err := s.freeze(ctx, d, now); err != nil {
			return err
		}
	}
	var next *job
	if err := s.step(func(dir *os.Root, set setting, st *state) error {
		var err error
		next, err = due(dir, st, telemetryconfig.Cutoff(set, st), s.at())
		return err
	}); err != nil || next == nil {
		return err
	}
	if s.cfg.beforeSend != nil {
		s.cfg.beforeSend()
	}
	return s.finish(*next, s.deliver(ctx, *next))
}

func (s *Sender) curlPresent() bool {
	if s.cfg.post != nil {
		return true
	}
	info, err := os.Stat(s.cfg.Curl)
	return err == nil && info.Mode().IsRegular()
}

func dayName(t time.Time) string { return t.UTC().Format(time.DateOnly) }

// toFreeze lists the closed days after the cutoff not frozen yet, oldest
// first.
func toFreeze(st *state, cutoff string, now time.Time) []time.Time {
	var days []time.Time
	today := now.UTC().Truncate(day)
	for d := today.AddDate(0, 0, -maxAgeDays); d.Before(today); d = d.AddDate(0, 0, 1) {
		if dayName(d) > cutoff && st.Days[dayName(d)] == nil {
			days = append(days, d)
		}
	}
	return days
}

// freeze builds day d once: the hook file is counted outside the store and
// the store read on its read-only connection, then the batch is written in
// a step if the day is still after the cutoff.
func (s *Sender) freeze(ctx context.Context, d, now time.Time) error {
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
	if err := s.db.View(ctx, func(tx store.Tx) error {
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
	return s.step(func(dir *os.Root, set setting, st *state) error {
		name, cutoff := dayName(d), telemetryconfig.Cutoff(set, st)
		if name <= cutoff || st.Days[name] != nil {
			return nil
		}
		if !r.Active() {
			st.Days[name] = &dayState{}
			return nil
		}
		// The week figures count only days after the cutoff.
		active, unobserved := int64(1), r.Unobserved10m
		for back := 1; back < 7; back++ {
			prior := dayName(d.AddDate(0, 0, -back))
			if p := st.Days[prior]; p != nil && p.Batch && prior > cutoff {
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

// recoverDays deletes unsent batches up to the cutoff, which an off or an
// opt-out whose cleanup failed can leave behind, and adopts batches after
// it that the state does not know, as after a restart between writing a
// batch and the state.
func recoverDays(dir *os.Root, st *state, cutoff string, now time.Time) error {
	pending, err := telemetryconfig.ListDays(dir, "pending")
	if err != nil {
		return err
	}
	for _, name := range pending {
		info := st.Days[name]
		if info != nil && info.Accepted {
			continue
		}
		keep := info != nil && name > cutoff
		var r telemetry.Report
		if info == nil && name > cutoff {
			body, err := telemetryconfig.ReadDayFile(dir, "pending", name)
			if err == nil {
				r, err = telemetry.Parse(body, now)
			}
			keep = err == nil
		}
		if !keep {
			if err := dir.Remove(filepath.Join("pending", name+".json")); err != nil {
				return err
			}
			continue
		}
		if info == nil {
			st.Days[name] = &dayState{Batch: true, Unobserved: r.Counts["notice.unobserved_10m"], NotBefore: now}
		}
	}
	// Unreadable sent/ only loses the week figures' memory of those days;
	// housekeeping reports it.
	sent, _ := telemetryconfig.ListDays(dir, "sent")
	for _, name := range sent {
		if st.Days[name] == nil && name > cutoff {
			st.Days[name] = &dayState{Batch: true, Accepted: true}
		}
	}
	return nil
}

type job struct {
	day  string
	body []byte
	now  time.Time
}

// due returns the oldest sendable batch when its time has come. A first
// attempt needs PostGap since the last one; a retry of the same batch does
// not.
func due(dir *os.Root, st *state, cutoff string, now time.Time) (*job, error) {
	days, err := telemetryconfig.ListDays(dir, "pending")
	if err != nil {
		return nil, err
	}
	for _, name := range days {
		if !st.Sendable(name, cutoff, now) {
			continue
		}
		if now.Before(st.NextAttempt(name)) {
			return nil, nil
		}
		body, err := telemetryconfig.ReadDayFile(dir, "pending", name)
		if err != nil {
			return nil, err
		}
		info := st.Days[name]
		if info.Attempts == 0 {
			st.LastPost = now
		}
		info.Attempts++
		return &job{day: name, body: body, now: now}, nil
	}
	return nil, nil
}

// result is what one request ended with.
type result struct {
	status     int // 0: no HTTP response
	retryAfter time.Duration
	// cancelled: the request was stopped or never started.
	cancelled bool
}

// sendable is the last check before curl and the watcher's check during
// the request: no opt-out (observe records one), the setting on, and the
// batch after the cutoff, pending and not accepted.
func (s *Sender) sendable(j job) bool {
	ok := false
	err := s.step(func(dir *os.Root, set setting, st *state) error {
		if !st.Sendable(j.day, telemetryconfig.Cutoff(set, st), s.at()) {
			return nil
		}
		_, err := dir.Lstat(filepath.Join("pending", j.day+".json"))
		ok = err == nil
		return nil
	})
	return ok && err == nil
}

// deliver runs one POST. It is cancelled as soon as the batch stops being
// sendable.
func (s *Sender) deliver(ctx context.Context, j job) result {
	if !s.sendable(j) {
		return result{cancelled: true}
	}
	if s.cfg.requestStarts != nil {
		s.cfg.requestStarts()
	}
	ctx, cancel := context.WithCancel(ctx)
	stopped := make(chan struct{})
	var watcher sync.WaitGroup
	defer func() {
		// No check outlives the request.
		cancel()
		watcher.Wait()
	}()
	watcher.Add(1)
	go func() {
		defer watcher.Done()
		ticker := time.NewTicker(s.cfg.watchEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !s.sendable(j) {
					close(stopped)
					cancel()
					return
				}
			}
		}
	}()
	post := s.curl
	if s.cfg.post != nil {
		post = s.cfg.post
	}
	res := post(ctx, j.body)
	select {
	case <-stopped:
		return result{cancelled: true}
	default:
	}
	if ctx.Err() != nil {
		return result{cancelled: true}
	}
	return res
}

func (s *Sender) curl(ctx context.Context, body []byte) result {
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
	cmd.Stdin = bytes.NewReader(body)
	headers := &limitedWriter{limit: 16 << 10}
	cmd.Stdout = headers
	cmd.WaitDelay = time.Second
	if err := cmd.Run(); err != nil {
		return result{}
	}
	return parseHeaders(headers.Bytes(), time.Now())
}

// finish records a request's outcome. A 204 is recorded whatever happened
// since, as the receiver holds the report; the bytes stay ineligible and
// expire keepSentDays after the acceptance.
func (s *Sender) finish(j job, res result) error {
	if res.status == 204 {
		return telemetryconfig.Locked(s.cfg.StateRoot, func(dir *os.Root, _ setting) error {
			st, err := telemetryconfig.ReadState(dir)
			if err != nil {
				return err
			}
			info := st.Days[j.day]
			if info == nil {
				info = &dayState{Batch: true}
				st.Days[j.day] = info
			}
			st.LastStatus, info.Accepted, info.AcceptedAt = "204", true, s.at()
			st.VetoDay = s.remember(st.VetoDay)
			// The acceptance is in the state before the move; a failed
			// move is finished later without a second request.
			if err := s.writeState(dir, st); err != nil {
				return err
			}
			return archive(dir, j.day, info.AcceptedAt)
		})
	}
	return s.step(func(dir *os.Root, set setting, st *state) error {
		info := st.Days[j.day]
		if res.cancelled || info == nil {
			return nil
		}
		if _, err := dir.Lstat(filepath.Join("pending", j.day+".json")); err != nil {
			return nil
		}
		return apply(dir, st, info, j, res, s.cfg.Version)
	})
}

// apply carries out the response table for every answer but 204.
func apply(dir *os.Root, st *state, info *dayState, j job, res result, version string) error {
	switch {
	case slices.Contains([]int{400, 413, 415, 422}, res.status):
		st.LastStatus = strconv.Itoa(res.status)
		return dir.Remove(filepath.Join("pending", j.day+".json"))
	case res.status == 410:
		st.LastStatus, st.StoppedVersion = "410", version
		return telemetryconfig.DropPending(dir, st)
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

// archive moves an accepted batch from pending/ to sent/. The copy's file
// time is its acceptance, from which it is kept keepSentDays.
//
// An opt-out that came while the request ran may have deleted the batch
// already; the acceptance is recorded and nothing is kept.
func archive(dir *os.Root, name string, accepted time.Time) error {
	body, err := telemetryconfig.ReadDayFile(dir, "pending", name)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := telemetryconfig.WriteDayFile(dir, "sent", name, body); err != nil {
		return err
	}
	if !accepted.IsZero() {
		if err := dir.Chtimes(filepath.Join("sent", name+".json"), accepted, accepted); err != nil {
			return err
		}
	}
	return dir.Remove(filepath.Join("pending", name+".json"))
}

// housekeeping runs before every decision, whatever the setting: it
// finishes owed archive moves, deletes accepted bytes keepSentDays after
// their acceptance wherever they are, deletes unsent batches older than
// maxAgeDays, and forgets days the week figures no longer need.
func housekeeping(dir *os.Root, st *state, now time.Time) error {
	today := now.UTC().Truncate(day)
	expired := now.Add(-keepSentDays * day)
	pending, err := telemetryconfig.ListDays(dir, "pending")
	if err != nil {
		return err
	}
	owed := map[string]bool{}
	var errs []error
	for _, name := range pending {
		path := filepath.Join("pending", name+".json")
		switch info := st.Days[name]; {
		case info != nil && info.Accepted && info.AcceptedAt.Before(expired):
			errs = append(errs, dir.Remove(path))
			delete(st.Days, name)
		case info != nil && info.Accepted:
			if err := archive(dir, name, info.AcceptedAt); err != nil {
				owed[name] = true
				errs = append(errs, err)
			}
		case name < dayName(today.AddDate(0, 0, -maxAgeDays)):
			errs = append(errs, dir.Remove(path))
		}
	}
	for name := range st.Days {
		if name < dayName(today.AddDate(0, 0, -maxAgeDays-6)) && !owed[name] {
			delete(st.Days, name)
		}
	}
	sent, err := telemetryconfig.ListDays(dir, "sent")
	if err != nil {
		return errors.Join(append(errs, err)...)
	}
	for _, name := range sent {
		path := filepath.Join("sent", name+".json")
		if info, err := dir.Lstat(path); err != nil {
			errs = append(errs, err)
		} else if info.ModTime().Before(expired) {
			errs = append(errs, dir.Remove(path))
		}
	}
	return errors.Join(errs...)
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
