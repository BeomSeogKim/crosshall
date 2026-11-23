package telemetrysend

import (
	"bytes"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/telemetry"
	"github.com/BeomSeogKim/crosshall/internal/telemetryconfig"
)

// d0 is midnight UTC of an invented day; the tests move a clock from it.
var d0 = time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)

// receiver is a local HTTPS endpoint that records every request.
type receiver struct {
	t      *testing.T
	srv    *httptest.Server
	caFile string
	mu     sync.Mutex
	bodies [][]byte
	// respond answers request n (from 0); nil answers 204.
	respond func(n int, w http.ResponseWriter, r *http.Request)
}

func newReceiver(t *testing.T) *receiver {
	rc := &receiver{t: t}
	rc.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rc.mu.Lock()
		n := len(rc.bodies)
		rc.bodies = append(rc.bodies, body)
		respond := rc.respond
		rc.mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/v1/report" || r.Header.Get("Content-Type") != "application/json" ||
			!strings.HasPrefix(r.Header.Get("User-Agent"), "crosshall/v0.2.0-rc.") || r.Header.Get("Cookie") != "" {
			t.Errorf("unexpected request %s %s %v", r.Method, r.URL.Path, r.Header)
		}
		if respond != nil {
			respond(n, w, r)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(rc.srv.Close)
	rc.caFile = filepath.Join(t.TempDir(), "ca.pem")
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rc.srv.Certificate().Raw})
	if err := os.WriteFile(rc.caFile, cert, 0600); err != nil {
		t.Fatal(err)
	}
	return rc
}

func (rc *receiver) received() [][]byte {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return slices.Clone(rc.bodies)
}

type hall struct {
	t     *testing.T
	state string
	rv    string
	db    *store.Store
	rc    *receiver
	cfg   Config
}

func newHall(t *testing.T) *hall {
	if _, err := os.Stat(telemetryconfig.CurlPath); err != nil {
		t.Skip("no " + telemetryconfig.CurlPath)
	}
	h := &hall{t: t, state: t.TempDir(), rv: filepath.Join(t.TempDir(), "rv"), rc: newReceiver(t)}
	if err := errors.Join(os.Chmod(h.state, 0700), os.Mkdir(h.rv, 0700)); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), h.state)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	h.db = db
	h.cfg = Config{
		StateRoot: h.state, Rendezvous: h.rv, Version: "v0.2.0-rc.2", Endpoint: h.rc.srv.URL + "/v1/report",
		PolicyPath: filepath.Join(t.TempDir(), "telemetry-off"), Curl: telemetryconfig.CurlPath,
		caFile: h.rc.caFile, timeout: 2 * time.Second,
	}
	return h
}

// setMode writes telemetry/mode as the CLI does, with a chosen time.
func (h *hall) setMode(mode string, at time.Time) {
	dir := filepath.Join(h.state, "telemetry")
	if err := os.MkdirAll(dir, 0700); err != nil {
		h.t.Fatal(err)
	}
	line := mode + " " + at.Format(time.RFC3339Nano) + " owner-command\n"
	if err := os.WriteFile(filepath.Join(dir, "mode"), []byte(line), 0600); err != nil {
		h.t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "mode"), 0600); err != nil {
		h.t.Fatal(err)
	}
}

// hookActivity records one hook sample on the day of at, the only activity
// a day needs to be reported.
func (h *hall) hookActivity(at time.Time) {
	rv, err := spool.OpenServerRoot(h.rv)
	if err != nil {
		h.t.Fatal(err)
	}
	defer rv.Close()
	dir, err := spool.OpenServerChild(rv, "hooks", true)
	if err != nil {
		h.t.Fatal(err)
	}
	defer dir.Close()
	f, err := spool.OpenServerFile(dir, at.UTC().Format("20060102"), os.O_WRONLY|os.O_CREATE|os.O_APPEND)
	if err != nil {
		h.t.Fatal(err)
	}
	defer f.Close()
	if _, err := io.WriteString(f, "claude UserPromptSubmit clean 3\n"); err != nil {
		h.t.Fatal(err)
	}
}

// tick runs one dispatcher tick of s and waits for its request.
func (h *hall) tick(s *Sender, now time.Time) {
	s.Tick(h.t.Context(), now)
	s.Wait()
}

// run ticks a fresh sender every hour from from up to to, as a dispatcher
// that runs throughout would.
func (h *hall) run(from, to time.Time) {
	s := New(h.cfg, h.db)
	for now := from; !now.After(to); now = now.Add(time.Hour) {
		h.tick(s, now)
	}
}

func (h *hall) files(sub string) []string {
	entries, err := os.ReadDir(filepath.Join(h.state, "telemetry", sub))
	if err != nil && !os.IsNotExist(err) {
		h.t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func reportOf(t *testing.T, body []byte, now time.Time) telemetry.Report {
	t.Helper()
	r, err := telemetry.Parse(body, now)
	if err != nil {
		t.Fatalf("invalid report %s: %v", body, err)
	}
	return r
}

// An answer at 23:59 sends nothing about that day; the first report is the
// next full day, sent after it closed, and a sent copy is kept.
func TestReportsOnlyWholeDaysAfterOn(t *testing.T) {
	h := newHall(t)
	h.setMode("on", d0.Add(23*time.Hour+59*time.Minute))
	h.hookActivity(d0.Add(23*time.Hour + 59*time.Minute + 30*time.Second))
	h.hookActivity(d0.Add(30 * time.Hour))
	h.run(d0.Add(24*time.Hour+time.Minute), d0.Add(54*time.Hour+time.Minute))
	got := h.rc.received()
	if len(got) != 1 {
		t.Fatalf("got %d reports, want 1", len(got))
	}
	r := reportOf(t, got[0], d0.Add(54*time.Hour))
	if r.Day != "2026-11-11" || r.Counts["hook.claude.prompt.clean"] != 1 || r.Counts["week.active_days"] != 1 {
		t.Fatalf("report = %+v", r)
	}
	if sent := h.files("sent"); !slices.Equal(sent, []string{"2026-11-11.json"}) {
		t.Fatalf("sent = %v", sent)
	}
	kept, err := os.ReadFile(filepath.Join(h.state, "telemetry", "sent", "2026-11-11.json"))
	if err != nil || string(kept) != string(got[0]) {
		t.Fatalf("sent copy %q differs from the bytes sent: %v", kept, err)
	}
	if pending := h.files("pending"); len(pending) != 0 {
		t.Fatalf("pending = %v", pending)
	}
}

// Off then on moves the start forward: no day before the second answer is
// ever built.
func TestOffThenOnSendsNoEarlierDay(t *testing.T) {
	h := newHall(t)
	for i := -3; i <= 1; i++ {
		h.hookActivity(d0.AddDate(0, 0, i).Add(time.Hour))
	}
	h.setMode("on", d0.AddDate(0, 0, -4))
	h.setMode("off", d0.AddDate(0, 0, -4).Add(time.Hour))
	h.setMode("on", d0.Add(12*time.Hour))
	h.run(d0.AddDate(0, 0, 2).Add(time.Minute), d0.AddDate(0, 0, 2).Add(7*time.Hour))
	got := h.rc.received()
	if len(got) != 1 || reportOf(t, got[0], d0.AddDate(0, 0, 2)).Day != "2026-11-11" {
		t.Fatalf("got %d reports %q, want only 2026-11-11", len(got), got)
	}
}

// A request that times out is resent with the same bytes and token, also by
// a dispatcher started after a restart.
func TestRetrySendsIdenticalBytes(t *testing.T) {
	h := newHall(t)
	h.setMode("on", d0.AddDate(0, 0, -2))
	h.hookActivity(d0.Add(-time.Hour))
	h.rc.respond = func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 0 {
			<-r.Context().Done() // past the client's timeout
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
	first := d0.Add(7 * time.Hour)
	h.tick(New(h.cfg, h.db), d0.Add(time.Minute))
	h.tick(New(h.cfg, h.db), first)
	if len(h.rc.received()) != 1 || len(h.files("pending")) != 1 {
		t.Fatalf("after the timeout: %d requests, pending %v", len(h.rc.received()), h.files("pending"))
	}
	restarted := New(h.cfg, h.db)
	h.tick(restarted, first.Add(59*time.Minute))
	h.tick(restarted, first.Add(61*time.Minute))
	got := h.rc.received()
	if len(got) != 2 || string(got[0]) != string(got[1]) {
		t.Fatalf("retry sent %q after %q", got[len(got)-1], got[0])
	}
	if len(h.files("pending")) != 0 || len(h.files("sent")) != 1 {
		t.Fatalf("pending %v sent %v", h.files("pending"), h.files("sent"))
	}
}

// Seven pending days go out one per 24 hours, oldest first.
func TestBacklogDrainsOnePerDay(t *testing.T) {
	h := newHall(t)
	h.setMode("on", d0.AddDate(0, 0, -9))
	for i := -7; i <= -1; i++ {
		h.hookActivity(d0.AddDate(0, 0, i).Add(time.Hour))
	}
	var times []time.Time
	var days []string
	s := New(h.cfg, h.db)
	for now := d0.Add(time.Minute); now.Before(d0.AddDate(0, 0, 9)); now = now.Add(time.Hour) {
		before := len(h.rc.received())
		h.tick(s, now)
		if got := h.rc.received(); len(got) > before {
			times = append(times, now)
			days = append(days, reportOf(t, got[len(got)-1], now).Day)
		}
	}
	want := []string{"2026-11-03", "2026-11-04", "2026-11-05", "2026-11-06", "2026-11-07", "2026-11-08", "2026-11-09"}
	if !slices.Equal(days, want) {
		t.Fatalf("sent days %v, want %v", days, want)
	}
	for i := 1; i < len(times); i++ {
		if times[i].Sub(times[i-1]) < 24*time.Hour {
			t.Fatalf("two requests within 24 hours: %v", times)
		}
	}
}

// A batch the receiver never accepts is deleted unsent once it is more than
// seven days old, and retries back off from one hour to a day.
func TestOldBatchIsDropped(t *testing.T) {
	h := newHall(t)
	h.setMode("on", d0.AddDate(0, 0, -2))
	h.hookActivity(d0.Add(-time.Hour))
	h.rc.respond = func(_ int, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }
	h.run(d0.Add(time.Minute), d0.AddDate(0, 0, 7).Add(time.Hour))
	if pending := h.files("pending"); len(pending) != 0 {
		t.Fatalf("pending = %v", pending)
	}
	if n := len(h.rc.received()); n < 7 || n > 12 {
		t.Fatalf("%d attempts in 7 days, want one an hour doubling to one a day", n)
	}
}

// Nothing is built, written or sent while the setting is unset or off, for a
// development build, or with an empty endpoint.
func TestNothingUnlessOn(t *testing.T) {
	for name, prepare := range map[string]func(h *hall){
		"unset":    func(h *hall) {},
		"off":      func(h *hall) { h.setMode("off", d0.AddDate(0, 0, -3)) },
		"dev":      func(h *hall) { h.setMode("on", d0.AddDate(0, 0, -3)); h.cfg.Version = "dev" },
		"endpoint": func(h *hall) { h.setMode("on", d0.AddDate(0, 0, -3)); h.cfg.Endpoint = "" },
		"no curl":  func(h *hall) { h.setMode("on", d0.AddDate(0, 0, -3)); h.cfg.Curl = "/nonexistent/curl" },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHall(t)
			h.hookActivity(d0.Add(-time.Hour))
			prepare(h)
			h.run(d0.Add(time.Minute), d0.Add(30*time.Hour))
			if n := len(h.rc.received()); n != 0 {
				t.Fatalf("%d requests", n)
			}
			for _, name := range []string{"state.json", "pending", "sent"} {
				if _, err := os.Lstat(filepath.Join(h.state, "telemetry", name)); !os.IsNotExist(err) {
					t.Fatalf("telemetry/%s exists: %v", name, err)
				}
			}
		})
	}
}

// off, the administrator's file and a dispatcher veto each stop a queued
// retry, delete the batch, and the suppressed days are never reported.
func TestOptOutsStopAQueuedRetry(t *testing.T) {
	for name, optOut := range map[string]func(h *hall){
		"off": func(h *hall) {
			if _, _, err := telemetryconfig.SetMode(h.state, telemetryconfig.Off, "owner-command", ""); err != nil {
				t.Fatal(err)
			}
		},
		"administrator": func(h *hall) {
			if err := os.WriteFile(h.cfg.PolicyPath, nil, 0644); err != nil {
				t.Fatal(err)
			}
		},
		"veto": func(h *hall) { h.cfg.Vetoed = true },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHall(t)
			h.setMode("on", d0.AddDate(0, 0, -2))
			h.hookActivity(d0.Add(-time.Hour))
			h.hookActivity(d0.Add(time.Hour))
			h.rc.respond = func(n int, w http.ResponseWriter, _ *http.Request) {
				if n == 0 {
					w.WriteHeader(http.StatusInternalServerError)
					return
				}
				w.WriteHeader(http.StatusNoContent)
			}
			s := New(h.cfg, h.db)
			now := d0.Add(time.Minute)
			for ; len(h.rc.received()) == 0; now = now.Add(time.Hour) {
				h.tick(s, now)
			}
			if len(h.rc.received()) != 1 || len(h.files("pending")) != 1 {
				t.Fatalf("before the opt-out: %d requests, pending %v", len(h.rc.received()), h.files("pending"))
			}
			optOut(h)
			// A dispatcher vetoed at launch runs one day, then one without
			// a veto runs on; the administrator's file is removed after a day.
			h.run(now, now.Add(24*time.Hour))
			os.Remove(h.cfg.PolicyPath)
			h.cfg.Vetoed = false
			h.run(now.Add(25*time.Hour), d0.AddDate(0, 0, 4))
			if n := len(h.rc.received()); n != 1 {
				t.Fatalf("%d requests after the opt-out", n)
			}
			if pending := h.files("pending"); len(pending) != 0 {
				t.Fatalf("pending = %v", pending)
			}
		})
	}
}

// Turning off while a request is in flight cancels it.
func TestOffDuringRequestCancelsIt(t *testing.T) {
	h := newHall(t)
	h.cfg.timeout = 30 * time.Second
	h.setMode("on", d0.AddDate(0, 0, -2))
	h.hookActivity(d0.Add(-time.Hour))
	started := make(chan struct{})
	h.rc.respond = func(_ int, w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-time.After(20 * time.Second):
			w.WriteHeader(http.StatusNoContent)
		}
	}
	s := New(h.cfg, h.db)
	h.tick(s, d0.Add(time.Minute))
	s.Tick(t.Context(), d0.Add(7*time.Hour))
	<-started
	begin := time.Now()
	if _, _, err := telemetryconfig.SetMode(h.state, telemetryconfig.Off, "owner-command", ""); err != nil {
		t.Fatal(err)
	}
	s.Wait()
	if elapsed := time.Since(begin); elapsed > 5*time.Second {
		t.Fatalf("request ran %v after off", elapsed)
	}
	if len(h.files("sent")) != 0 || len(h.files("pending")) != 0 {
		t.Fatalf("sent %v pending %v", h.files("sent"), h.files("pending"))
	}
}

// 400 deletes the batch; 410 stops sending until the version changes.
func TestRejectedAndGone(t *testing.T) {
	h := newHall(t)
	h.setMode("on", d0.AddDate(0, 0, -3))
	for i := -2; i <= 2; i++ {
		h.hookActivity(d0.AddDate(0, 0, i).Add(time.Hour))
	}
	h.rc.respond = func(n int, w http.ResponseWriter, _ *http.Request) {
		if n == 0 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusGone)
	}
	h.run(d0.Add(time.Minute), d0.AddDate(0, 0, 4))
	if n := len(h.rc.received()); n != 2 {
		t.Fatalf("%d requests, want a 400 and then a 410", n)
	}
	if pending := h.files("pending"); len(pending) != 0 {
		t.Fatalf("pending = %v", pending)
	}
	var st state
	raw, _ := os.ReadFile(filepath.Join(h.state, "telemetry", "state.json"))
	if err := json.Unmarshal(raw, &st); err != nil || st.StoppedVersion != "v0.2.0-rc.2" {
		t.Fatalf("state %s: %v", raw, err)
	}
	h.rc.respond = nil
	h.cfg.Version = "v0.2.0-rc.3"
	h.hookActivity(d0.AddDate(0, 0, 4).Add(time.Hour))
	h.run(d0.AddDate(0, 0, 5).Add(time.Minute), d0.AddDate(0, 0, 6))
	if n := len(h.rc.received()); n != 3 {
		t.Fatalf("%d requests after the upgrade, want 3", n)
	}
}

func TestRetryAfter(t *testing.T) {
	now := d0
	for raw, want := range map[string]time.Duration{
		"HTTP/1.1 429 Too Many Requests\r\nRetry-After: 120\r\n\r\n":                           2 * time.Minute,
		"HTTP/1.1 503 x\r\nretry-after: 999999\r\n\r\n":                                        24 * time.Hour,
		"HTTP/1.1 503 x\r\nRetry-After: " + now.Add(3*time.Hour).Format(time.RFC1123) + "\r\n": 3 * time.Hour,
		"HTTP/1.1 503 x\r\n\r\n": 0,
	} {
		if got := parseHeaders([]byte(raw), now); got.retryAfter != want {
			t.Errorf("%q: retry after %v, want %v", raw, got.retryAfter, want)
		}
	}
}

// A batch selected for sending is not sent once off and on started a new
// consent interval, even though the request was already under way.
func TestOffAndOnAfterSelectionSendsNothingOld(t *testing.T) {
	h := newHall(t)
	h.setMode("on", d0.AddDate(0, 0, -2))
	h.hookActivity(d0.Add(-time.Hour))
	h.cfg.beforeSend = func() {
		for _, mode := range []string{telemetryconfig.Off, telemetryconfig.On} {
			if _, _, err := telemetryconfig.SetMode(h.state, mode, "owner-command", ""); err != nil {
				t.Error(err)
			}
		}
	}
	h.run(d0.Add(time.Minute), d0.AddDate(0, 0, 3))
	if n := len(h.rc.received()); n != 0 {
		t.Fatalf("%d requests after off and on", n)
	}
}

// The administrator's file seen during a request cancels it and is
// remembered: after the file is gone and the dispatcher restarted, neither
// the cancelled batch nor the day the file was seen on is sent.
func TestOptOutSeenDuringRequestOutlivesIt(t *testing.T) {
	h := newHall(t)
	h.setMode("on", d0.AddDate(0, 0, -2))
	for i := -1; i <= 1; i++ {
		h.hookActivity(d0.AddDate(0, 0, i).Add(time.Hour))
	}
	h.rc.respond = func(n int, w http.ResponseWriter, r *http.Request) {
		if n == 0 {
			if err := os.WriteFile(h.cfg.PolicyPath, nil, 0644); err != nil {
				t.Error(err)
			}
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
	h.cfg.timeout = 30 * time.Second
	s := New(h.cfg, h.db)
	now := d0.Add(time.Minute)
	for ; len(h.rc.received()) == 0; now = now.Add(time.Hour) {
		h.tick(s, now)
	}
	if pending := h.files("pending"); len(pending) != 0 {
		t.Fatalf("pending after the opt-out = %v", pending)
	}
	if err := os.Remove(h.cfg.PolicyPath); err != nil {
		t.Fatal(err)
	}
	h.run(now, d0.AddDate(0, 0, 3))
	var days []string
	for _, body := range h.rc.received()[1:] {
		days = append(days, reportOf(t, body, d0.AddDate(0, 0, 3)).Day)
	}
	if !slices.Equal(days, []string{"2026-11-11"}) {
		t.Fatalf("sent after the opt-out: %v, want only the day after it", days)
	}
}

// A missing curl does not skip the opt-out bookkeeping.
func TestOptOutIsRecordedWithoutCurl(t *testing.T) {
	h := newHall(t)
	h.setMode("on", d0.AddDate(0, 0, -2))
	h.cfg.Curl = "/nonexistent/curl"
	h.cfg.Vetoed = true
	h.tick(New(h.cfg, h.db), d0.Add(time.Hour))
	st := h.readState(t)
	if at := st.SuppressedThrough; at.Before(d0.Add(time.Hour)) || at.After(d0.Add(time.Hour+time.Minute)) {
		t.Fatalf("suppressed through %v", st.SuppressedThrough)
	}
}

// After off and on, the week figures count only days of the new interval.
func TestWeekFiguresStayInTheInterval(t *testing.T) {
	h := newHall(t)
	h.setMode("on", d0.AddDate(0, 0, -4))
	for _, i := range []int{-3, -2, 0} {
		h.hookActivity(d0.AddDate(0, 0, i).Add(time.Hour))
	}
	h.run(d0.AddDate(0, 0, -2).Add(time.Minute), d0.AddDate(0, 0, -1))
	h.setMode("on", d0.AddDate(0, 0, -1).Add(2*time.Hour))
	h.run(d0.AddDate(0, 0, 1).Add(time.Minute), d0.AddDate(0, 0, 2).Add(7*time.Hour))
	got := h.rc.received()
	last := reportOf(t, got[len(got)-1], d0.AddDate(0, 0, 2))
	if last.Day != "2026-11-10" || last.Counts["week.active_days"] != 1 {
		t.Fatalf("last report %+v", last)
	}
}

// A seven-day backlog of busy hook files is built beside the dispatcher:
// Tick returns at once and the store is never held longer than one day's
// queries, well inside a hook's 150 ms deadline.
func TestBacklogDoesNotHoldTheDispatcher(t *testing.T) {
	h := newHall(t)
	h.setMode("on", d0.AddDate(0, 0, -9))
	line := []byte("claude UserPromptSubmit clean 3\n")
	for i := -7; i <= -1; i++ {
		h.hookActivity(d0.AddDate(0, 0, i))
		path := filepath.Join(h.rv, "hooks", d0.AddDate(0, 0, i).Format("20060102"))
		if err := os.WriteFile(path, bytes.Repeat(line, 50_000), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s := New(h.cfg, h.db)
	begin := time.Now()
	s.Tick(t.Context(), d0.Add(time.Minute))
	if took := time.Since(begin); took > 20*time.Millisecond {
		t.Fatalf("Tick blocked for %v", took)
	}
	var worst time.Duration
	done := make(chan struct{})
	go func() { s.Wait(); close(done) }()
	for running := true; running; {
		select {
		case <-done:
			running = false
		default:
			start := time.Now()
			if err := h.db.Update(t.Context(), func(store.Tx) error { return nil }); err != nil {
				t.Fatal(err)
			}
			worst = max(worst, time.Since(start))
			time.Sleep(time.Millisecond)
		}
	}
	t.Logf("the store was held at most %v", worst)
	if worst > 100*time.Millisecond {
		t.Fatalf("the store was held for %v", worst)
	}
	if pending := h.files("pending"); len(pending) != 7 {
		t.Fatalf("pending = %v", pending)
	}
}

// A restart between writing a batch and the state adopts the batch's bytes
// instead of building the day again.
func TestRestartAdoptsFrozenBytes(t *testing.T) {
	h := newHall(t)
	h.setMode("on", d0.AddDate(0, 0, -2))
	h.hookActivity(d0.Add(-time.Hour))
	h.tick(New(h.cfg, h.db), d0.Add(time.Minute))
	frozen, err := os.ReadFile(filepath.Join(h.state, "telemetry", "pending", "2026-11-09.json"))
	if err != nil || reportOf(t, frozen, d0).Sources.Snapshot != "taken" {
		t.Fatalf("frozen %s: %v", frozen, err)
	}
	if err := os.Remove(filepath.Join(h.state, "telemetry", "state.json")); err != nil {
		t.Fatal(err)
	}
	h.run(d0.Add(time.Hour), d0.Add(8*time.Hour))
	if got := h.rc.received(); len(got) != 1 || !bytes.Equal(got[0], frozen) {
		t.Fatalf("sent %q, want the frozen %q", got, frozen)
	}
}

// A 204 is recorded before the batch is moved; a failed move is finished
// later without a second request.
func TestAcceptedBatchIsNotSentTwice(t *testing.T) {
	h := newHall(t)
	h.setMode("on", d0.AddDate(0, 0, -2))
	h.hookActivity(d0.Add(-time.Hour))
	blocker := filepath.Join(h.state, "telemetry", "sent")
	if err := os.WriteFile(blocker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	h.run(d0.Add(time.Minute), d0.Add(10*time.Hour))
	if n := len(h.rc.received()); n != 1 {
		t.Fatalf("%d requests while the move failed", n)
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	h.run(d0.Add(11*time.Hour), d0.Add(12*time.Hour))
	if n := len(h.rc.received()); n != 1 || len(h.files("pending")) != 0 || len(h.files("sent")) != 1 {
		t.Fatalf("%d requests, pending %v, sent %v", n, h.files("pending"), h.files("sent"))
	}
}

// Sent copies are deleted 30 days after their acceptance in every state.
func TestSentCopiesExpireWhileOff(t *testing.T) {
	for name, prepare := range map[string]func(h *hall){
		"off":           func(h *hall) { h.setMode("off", d0.AddDate(0, 0, -40)) },
		"administrator": func(h *hall) { h.setMode("on", d0.AddDate(0, 0, -40)); os.WriteFile(h.cfg.PolicyPath, nil, 0644) },
		"stopped": func(h *hall) {
			h.setMode("on", d0.AddDate(0, 0, -40))
			dir, err := os.OpenRoot(filepath.Join(h.state, "telemetry"))
			if err != nil {
				t.Fatal(err)
			}
			defer dir.Close()
			if err := telemetryconfig.WriteState(dir, &state{StoppedVersion: h.cfg.Version}); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHall(t)
			prepare(h)
			sent := filepath.Join(h.state, "telemetry", "sent")
			if err := os.Mkdir(sent, 0700); err != nil {
				t.Fatal(err)
			}
			// Copies are kept by their acceptance, the file's time.
			for d, accepted := range map[string]time.Time{"2026-09-26": d0.AddDate(0, 0, -45), "2026-10-20": d0.AddDate(0, 0, -20)} {
				path := filepath.Join(sent, d+".json")
				if err := errors.Join(os.WriteFile(path, []byte("{}"), 0600), os.Chtimes(path, accepted, accepted)); err != nil {
					t.Fatal(err)
				}
			}
			h.tick(New(h.cfg, h.db), d0)
			if got := h.files("sent"); !slices.Equal(got, []string{"2026-10-20.json"}) {
				t.Fatalf("sent = %v", got)
			}
		})
	}
}

// The header buffer keeps its cap when exec copies the child's output.
func TestHeaderBufferIsCapped(t *testing.T) {
	w := &limitedWriter{limit: 16 << 10}
	if _, err := io.Copy(w, bytes.NewReader(make([]byte, 1<<20))); err != nil {
		t.Fatal(err)
	}
	if len(w.Bytes()) != 16<<10 {
		t.Fatalf("kept %d bytes", len(w.Bytes()))
	}
}

func (h *hall) readState(t *testing.T) *state {
	t.Helper()
	dir, err := os.OpenRoot(filepath.Join(h.state, "telemetry"))
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	st, err := telemetryconfig.ReadState(dir)
	if err != nil {
		t.Fatal(err)
	}
	return st
}
