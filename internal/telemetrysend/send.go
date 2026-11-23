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
	"io/fs"
	"log"
	"math/big"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/ident"
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
	// postGap is the least time between two first attempts.
	postGap = 24 * time.Hour
	// sendSpread is how far ahead a new batch's send time is drawn.
	sendSpread = 6 * time.Hour
	firstRetry = time.Hour
	lastRetry  = 24 * time.Hour
	maxBody    = 16 << 10
	day        = 24 * time.Hour
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
	// caFile and timeout are set only by tests.
	caFile  string
	timeout time.Duration
}

// Sender is driven by the dispatcher loop: Tick at start and once a minute.
type Sender struct {
	cfg     Config
	db      *store.Store
	mu      sync.Mutex
	sending bool
	wg      sync.WaitGroup
}

// New returns a sender for one dispatcher.
func New(cfg Config, db *store.Store) *Sender {
	if cfg.timeout == 0 {
		cfg.timeout = 10 * time.Second
	}
	return &Sender{cfg: cfg, db: db}
}

// Wait returns when no request is in flight. Cancel the context given to
// Tick first.
func (s *Sender) Wait() { s.wg.Wait() }

// state is telemetry/state.json. Days holds every day frozen in the last
// eight, with or without a batch; a day in it is never built again.
type state struct {
	SuppressedThrough time.Time           `json:"suppressed_through,omitzero"`
	Days              map[string]*dayInfo `json:"days,omitempty"`
	LastPost          time.Time           `json:"last_post,omitzero"`
	StoppedVersion    string              `json:"stopped_version,omitempty"`
	LastStatus        string              `json:"last_status,omitempty"`
}

type dayInfo struct {
	Batch bool `json:"batch"`
	// Unobserved is the day's notice.unobserved_10m before rounding, for
	// later days' week figures.
	Unobserved int64     `json:"unobserved_10m,omitempty"`
	NotBefore  time.Time `json:"not_before,omitzero"`
	Attempts   int       `json:"attempts,omitempty"`
}

type job struct {
	day  string
	body []byte
	now  time.Time
}

// Tick freezes every closed eligible day and starts the next due request.
// Failures are logged; nothing is sent while anything is in doubt.
func (s *Sender) Tick(ctx context.Context, now time.Time) {
	if err := s.tick(ctx, now); err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("dispatcher: usage report: %v", err)
	}
}

func (s *Sender) tick(ctx context.Context, now time.Time) error {
	if s.cfg.Endpoint == "" || telemetry.AppVersion(s.cfg.Version) == "dev" || !s.curlPresent() {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var next *job
	err := telemetryconfig.Locked(s.cfg.StateRoot, func(dir *os.Root, setting telemetryconfig.Setting) error {
		if setting.Mode != telemetryconfig.On {
			return nil
		}
		st, err := readState(dir)
		if err != nil {
			return err
		}
		if s.suppressed() {
			// Days this dispatcher saw suppressed are never reported.
			st.SuppressedThrough = now
			return errors.Join(dir.RemoveAll("pending"), writeState(dir, st))
		}
		if st.StoppedVersion == s.cfg.Version {
			return dir.RemoveAll("pending")
		}
		if err := prune(dir, st, now); err != nil {
			return err
		}
		if err := s.freeze(ctx, dir, st, setting, now); err != nil {
			return errors.Join(err, writeState(dir, st))
		}
		if !s.sending {
			if next, err = due(dir, st, now); err != nil {
				return err
			}
		}
		return writeState(dir, st)
	})
	if err != nil || next == nil {
		return err
	}
	s.sending = true
	s.wg.Add(1)
	go s.post(ctx, *next)
	return nil
}

func (s *Sender) curlPresent() bool {
	info, err := os.Stat(s.cfg.Curl)
	return err == nil && info.Mode().IsRegular()
}

func (s *Sender) suppressed() bool {
	return s.cfg.Vetoed || telemetryconfig.ManagedOffAt(s.cfg.PolicyPath)
}

// on reads the effective setting again, as before every request.
func (s *Sender) on() bool {
	setting, err := telemetryconfig.ReadMode(s.cfg.StateRoot)
	return err == nil && setting.Mode == telemetryconfig.On && !s.suppressed()
}

func dayName(t time.Time) string { return t.UTC().Format(time.DateOnly) }

// freeze builds each closed day that lies wholly after the setting turned on
// and after the last suppression seen, at most maxAgeDays back. A day with
// no activity is recorded without a batch.
func (s *Sender) freeze(ctx context.Context, dir *os.Root, st *state, setting telemetryconfig.Setting, now time.Time) error {
	from := setting.ChangedAt
	if st.SuppressedThrough.After(from) {
		from = st.SuppressedThrough
	}
	eligible := telemetryconfig.NextFullDay(from)
	today := now.UTC().Truncate(day)
	for d := today.AddDate(0, 0, -maxAgeDays); d.Before(today); d = d.AddDate(0, 0, 1) {
		if d.Before(eligible) || st.Days[dayName(d)] != nil {
			continue
		}
		info, body, err := s.build(ctx, st, d, now)
		if err != nil {
			return err
		}
		if body != nil {
			if err := writePrivate(dir, "pending", dayName(d)+".json", body); err != nil {
				return err
			}
		}
		st.Days[dayName(d)] = info
	}
	return nil
}

func (s *Sender) build(ctx context.Context, st *state, d, now time.Time) (*dayInfo, []byte, error) {
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return nil, nil, err
	}
	in := telemetry.Input{Day: d, Batch: hex.EncodeToString(token[:]), Version: s.cfg.Version, Now: now}
	if rv, err := spool.OpenServerRoot(s.cfg.Rendezvous); err == nil {
		in.Hooks, in.HooksRead = telemetry.ReadHookFile(rv, d)
		rv.Close()
	}
	var r telemetry.Report
	err := s.db.Update(ctx, func(tx store.Tx) error {
		first, err := telemetry.Build(ctx, tx, in)
		if err != nil || !first.Active() {
			r = first
			return err
		}
		// The week figures include this day, which counts once it is active.
		in.WeekActiveDays, in.WeekUnobserved10m = 1, first.Unobserved10m
		for back := 1; back < 7; back++ {
			if prior := st.Days[dayName(d.AddDate(0, 0, -back))]; prior != nil && prior.Batch {
				in.WeekActiveDays++
				in.WeekUnobserved10m += prior.Unobserved
			}
		}
		r, err = telemetry.Build(ctx, tx, in)
		return err
	})
	if err != nil {
		return nil, nil, err
	}
	if !r.Active() {
		return &dayInfo{}, nil, nil
	}
	body, err := r.Marshal()
	if err == nil {
		err = telemetry.Validate(body, now)
	}
	if err == nil && len(body) > maxBody {
		err = fmt.Errorf("report is %d bytes", len(body))
	}
	if err != nil {
		// Never retried: the day stays recorded without a batch.
		log.Printf("dispatcher: usage report for %s not frozen: %v", dayName(d), err)
		return &dayInfo{}, nil, nil
	}
	spread, err := rand.Int(rand.Reader, big.NewInt(int64(sendSpread)))
	if err != nil {
		return nil, nil, err
	}
	return &dayInfo{Batch: true, Unobserved: r.Unobserved10m, NotBefore: now.Add(time.Duration(spread.Int64()))}, body, nil
}

// due returns the oldest pending batch when its time has come. A first
// attempt needs postGap since the last one; a retry of the same batch does
// not.
func due(dir *os.Root, st *state, now time.Time) (*job, error) {
	days, err := listDays(dir, "pending")
	if err != nil || len(days) == 0 {
		return nil, err
	}
	name := days[0]
	info := st.Days[name]
	if info == nil {
		// A batch the state does not know is not sent.
		return nil, dir.Remove("pending/" + name + ".json")
	}
	if now.Before(info.NotBefore) || info.Attempts == 0 && now.Before(st.LastPost.Add(postGap)) {
		return nil, nil
	}
	pending, err := ident.OpenPrivateChild(dir, "pending")
	if err != nil {
		return nil, err
	}
	defer pending.Close()
	body, err := ident.ReadPrivateFile(pending, name+".json", maxBody)
	if err != nil {
		return nil, err
	}
	if info.Attempts == 0 {
		st.LastPost = now
	}
	info.Attempts++
	return &job{day: name, body: body, now: now}, nil
}

// result is what one request ended with.
type result struct {
	status     int // 0: no HTTP response
	retryAfter time.Duration
	cancelled  bool
}

func (s *Sender) post(ctx context.Context, j job) {
	defer s.wg.Done()
	res := s.deliver(ctx, j.body)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sending = false
	err := telemetryconfig.Locked(s.cfg.StateRoot, func(dir *os.Root, setting telemetryconfig.Setting) error {
		if setting.Mode != telemetryconfig.On || res.cancelled {
			// Turning off removed the batch; a shutdown sends it again later.
			return nil
		}
		st, err := readState(dir)
		if err != nil {
			return err
		}
		return errors.Join(apply(dir, st, j, res, s.cfg.Version), writeState(dir, st))
	})
	if err != nil {
		log.Printf("dispatcher: usage report: %v", err)
	}
}

// apply carries out the response table.
func apply(dir *os.Root, st *state, j job, res result, version string) error {
	pending := "pending/" + j.day + ".json"
	switch {
	case res.status == 204:
		st.LastStatus = "204"
		if err := writePrivate(dir, "sent", j.day+".json", j.body); err != nil {
			return err
		}
		return dir.Remove(pending)
	case slices.Contains([]int{400, 413, 415, 422}, res.status):
		st.LastStatus = strconv.Itoa(res.status)
		return dir.Remove(pending)
	case res.status == 410:
		st.LastStatus, st.StoppedVersion = "410", version
		return dir.RemoveAll("pending")
	}
	st.LastStatus = "failed"
	if res.status != 0 {
		st.LastStatus = strconv.Itoa(res.status)
	}
	info := st.Days[j.day]
	if info == nil {
		return nil
	}
	wait := firstRetry << min(info.Attempts-1, 5)
	if res.retryAfter > 0 {
		wait = res.retryAfter
	}
	info.NotBefore = j.now.Add(min(wait, lastRetry))
	return nil
}

// deliver runs one POST through curl. It is cancelled as soon as the
// effective setting stops being on.
func (s *Sender) deliver(ctx context.Context, body []byte) result {
	if !s.on() {
		return result{cancelled: true}
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	off := make(chan struct{})
	go func() {
		ticker := time.NewTicker(200 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if !s.on() {
					close(off)
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
	cmd.Stdin = bytes.NewReader(body)
	var headers limitedBuffer
	cmd.Stdout = &headers
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	select {
	case <-off:
		return result{cancelled: true}
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

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := 16<<10 - b.Len(); room > 0 {
		b.Buffer.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

// prune deletes batches older than maxAgeDays unsent, forgets days older
// than the week figures need, and deletes sent copies after keepSentDays.
func prune(dir *os.Root, st *state, now time.Time) error {
	today := now.UTC().Truncate(day)
	oldest := dayName(today.AddDate(0, 0, -maxAgeDays))
	days, err := listDays(dir, "pending")
	if err != nil {
		return err
	}
	for _, name := range days {
		if name < oldest {
			if err := dir.Remove("pending/" + name + ".json"); err != nil {
				return err
			}
		}
	}
	for name := range st.Days {
		if name < dayName(today.AddDate(0, 0, -maxAgeDays-6)) {
			delete(st.Days, name)
		}
	}
	sent, err := listDays(dir, "sent")
	if err != nil {
		return err
	}
	for _, name := range sent {
		if name < dayName(today.AddDate(0, 0, -keepSentDays)) {
			if err := dir.Remove("sent/" + name + ".json"); err != nil {
				return err
			}
		}
	}
	return nil
}

// listDays returns the days of sub's day files in ascending order.
func listDays(dir *os.Root, sub string) ([]string, error) {
	child, err := ident.OpenPrivateChild(dir, sub)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer child.Close()
	entries, err := fs.ReadDir(child.FS(), ".")
	if err != nil {
		return nil, err
	}
	var days []string
	for _, entry := range entries {
		name, ok := strings.CutSuffix(entry.Name(), ".json")
		if _, err := time.Parse(time.DateOnly, name); ok && err == nil && entry.Type().IsRegular() {
			days = append(days, name)
		}
	}
	slices.Sort(days)
	return days, nil
}

func readState(dir *os.Root) (*state, error) {
	st := &state{}
	raw, err := ident.ReadPrivateFile(dir, "state.json", 64<<10)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err == nil {
		if err := json.Unmarshal(raw, st); err != nil {
			return nil, fmt.Errorf("telemetry/state.json: %w", err)
		}
	}
	if st.Days == nil {
		st.Days = make(map[string]*dayInfo)
	}
	return st, nil
}

func writeState(dir *os.Root, st *state) error {
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return writeFile(dir, "state.json", raw)
}

// writePrivate writes name into the private subdirectory sub, creating it.
func writePrivate(dir *os.Root, sub, name string, data []byte) error {
	if err := dir.Mkdir(sub, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	child, err := ident.OpenPrivateChild(dir, sub)
	if err != nil {
		return err
	}
	defer child.Close()
	return writeFile(child, name, data)
}

// writeFile replaces name atomically with a 0600 file.
func writeFile(dir *os.Root, name string, data []byte) error {
	stage := name + "-" + rand.Text() + ".part"
	file, err := dir.OpenFile(stage, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer dir.Remove(stage)
	_, err = file.Write(data)
	if err := errors.Join(err, file.Sync(), file.Close()); err != nil {
		return err
	}
	return dir.Rename(stage, name)
}
