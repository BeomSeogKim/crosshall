package telemetrysend

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/telemetryconfig"
)

func (h *hall) sentDays(time.Time) []string {
	var days []string
	for _, body := range h.rc.received() {
		var r struct{ Day string }
		if err := json.Unmarshal(body, &r); err != nil {
			h.t.Fatal(err)
		}
		days = append(days, r.Day)
	}
	return days
}

// An off that was saved while removing the queue failed is still a cutoff:
// after on again and a restart, no earlier day is sent.
func TestOffIsACutoffEvenWhenCleanupFailed(t *testing.T) {
	for _, optOut := range []string{"off", "administrator"} {
		t.Run(optOut, func(t *testing.T) {
			h := newHall(t)
			h.setMode("on", d0.AddDate(0, 0, -3))
			h.hookActivity(d0.AddDate(0, 0, -2).Add(time.Hour))
			h.hookActivity(d0.AddDate(0, 0, 1).Add(time.Hour))
			h.tick(New(h.cfg, h.db), d0.AddDate(0, 0, -1).Add(time.Minute))
			pending := filepath.Join(h.state, "telemetry", "pending")
			if len(h.files("pending")) != 1 {
				t.Fatalf("pending = %v", h.files("pending"))
			}
			if err := os.Chmod(pending, 0500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(pending, 0700) })
			if optOut == "off" {
				if _, _, err := telemetryconfig.SetMode(h.state, telemetryconfig.Off, "owner-command", ""); err == nil {
					t.Fatal("removing the queue did not fail")
				}
				h.setMode("on", d0.AddDate(0, 0, -1).Add(2*time.Hour))
			} else {
				if err := os.WriteFile(h.cfg.PolicyPath, nil, 0644); err != nil {
					t.Fatal(err)
				}
				h.tick(New(h.cfg, h.db), d0.AddDate(0, 0, -1).Add(2*time.Hour))
				os.Remove(h.cfg.PolicyPath)
			}
			if err := os.Chmod(pending, 0700); err != nil {
				t.Fatal(err)
			}
			h.run(d0.AddDate(0, 0, -1).Add(3*time.Hour), d0.AddDate(0, 0, 3))
			if days := h.sentDays(d0.AddDate(0, 0, 3)); !slices.Equal(days, []string{"2026-11-11"}) {
				t.Fatalf("sent %v, want only 2026-11-11", days)
			}
		})
	}
}

// The administrator's file that first appears while a round builds is
// recorded at once: the queue is emptied and the day is never sent.
func TestOptOutSeenDuringBuildIsRecorded(t *testing.T) {
	h := newHall(t)
	h.setMode("on", d0.AddDate(0, 0, -4))
	for _, i := range []int{-3, -2, -1, 1} {
		h.hookActivity(d0.AddDate(0, 0, i).Add(time.Hour))
	}
	h.tick(New(h.cfg, h.db), d0.AddDate(0, 0, -2).Add(time.Minute))
	if len(h.files("pending")) != 1 {
		t.Fatalf("pending = %v", h.files("pending"))
	}
	during := h.cfg
	during.afterPrepare = func() {
		if err := os.WriteFile(h.cfg.PolicyPath, nil, 0644); err != nil {
			t.Error(err)
		}
	}
	h.tick(New(during, h.db), d0.Add(time.Minute))
	if st := h.readState(t); st.VetoDay != "2026-11-10" || len(h.files("pending")) != 0 {
		t.Fatalf("veto day %q, pending %v", st.VetoDay, h.files("pending"))
	}
	os.Remove(h.cfg.PolicyPath)
	h.run(d0.Add(time.Hour), d0.AddDate(0, 0, 3))
	if days := h.sentDays(d0.AddDate(0, 0, 3)); !slices.Equal(days, []string{"2026-11-11"}) {
		t.Fatalf("sent %v, want only 2026-11-11", days)
	}
}

// Building a day reads the store on a connection of its own: with a large
// ledger a competing write transaction does not wait for the build.
func TestBuildDoesNotHoldTheStore(t *testing.T) {
	h := newHall(t)
	h.setMode("on", d0.AddDate(0, 0, -9))
	h.hookActivity(d0.AddDate(0, 0, -1))
	if err := h.db.Update(t.Context(), func(tx store.Tx) error {
		_, err := tx.ExecContext(t.Context(), `WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x + 1 FROM n WHERE x < 500000)
			INSERT INTO event(kind, subject, detail, at_ms) SELECT 'joined', 'synthetic', '{}', ?1 + (x % 180) * 86400000 FROM n`,
			d0.AddDate(0, 0, -180).UnixMilli())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	s := New(h.cfg, h.db)
	s.Tick(t.Context(), d0.Add(time.Minute))
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
	t.Logf("a write transaction waited at most %v", worst)
	if worst > 50*time.Millisecond {
		t.Fatalf("a write transaction waited %v for the build", worst)
	}
	if len(h.files("pending")) != 7 {
		t.Fatalf("pending = %v", h.files("pending"))
	}
}

// A batch the receiver accepted while the archive was unwritable survives
// the next midnight, when it is older than the queue keeps, and its copy is
// kept 30 days from its acceptance, not from its day.
func TestAcceptedBatchSurvivesAgeing(t *testing.T) {
	h := newHall(t)
	h.setMode("on", d0.AddDate(0, 0, -9))
	h.hookActivity(d0.AddDate(0, 0, -7).Add(time.Hour))
	blocker := filepath.Join(h.state, "telemetry", "sent")
	if err := os.WriteFile(blocker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	h.run(d0.Add(time.Minute), d0.Add(7*time.Hour))
	if len(h.rc.received()) != 1 {
		t.Fatalf("%d requests", len(h.rc.received()))
	}
	if err := os.Remove(blocker); err != nil {
		t.Fatal(err)
	}
	h.tick(New(h.cfg, h.db), d0.AddDate(0, 0, 1).Add(time.Minute))
	if sent := h.files("sent"); !slices.Equal(sent, []string{"2026-11-03.json"}) {
		t.Fatalf("sent = %v, pending = %v", sent, h.files("pending"))
	}
	h.tick(New(h.cfg, h.db), d0.AddDate(0, 0, 25))
	if sent := h.files("sent"); len(sent) != 1 {
		t.Fatalf("sent copy gone %v after its acceptance, %d days after its day", 25*24*time.Hour, 32)
	}
	h.tick(New(h.cfg, h.db), d0.AddDate(0, 0, 31))
	if sent := h.files("sent"); len(sent) != 0 {
		t.Fatalf("sent = %v after 31 days", sent)
	}
	if len(h.rc.received()) != 1 {
		t.Fatalf("%d requests", len(h.rc.received()))
	}
}

// A veto first seen just after midnight, in the last check before the
// request, is recorded on the new day: the round started the evening
// before, and the new day is never sent.
func TestVetoAfterMidnightIsRecordedOnItsDay(t *testing.T) {
	h := newHall(t)
	h.setMode("on", d0.AddDate(0, 0, -3))
	h.hookActivity(d0.AddDate(0, 0, -2).Add(time.Hour))
	h.hookActivity(d0.Add(time.Hour))
	h.hookActivity(d0.AddDate(0, 0, 1).Add(time.Hour))
	var mu sync.Mutex
	clock := d0.Add(-400 * time.Millisecond)
	cfg := h.cfg
	cfg.clock = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	// A batch for November 8 is due when the round starts.
	h.tick(New(h.cfg, h.db), d0.AddDate(0, 0, -1).Add(time.Minute))
	h.rc.respond = func(_ int, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }
	h.tick(New(h.cfg, h.db), d0.AddDate(0, 0, -1).Add(7*time.Hour))
	h.rc.respond = nil
	before := len(h.rc.received())
	cfg.beforeSend = func() {
		mu.Lock()
		clock = d0.Add(200 * time.Millisecond)
		mu.Unlock()
		if err := os.WriteFile(h.cfg.PolicyPath, nil, 0644); err != nil {
			t.Error(err)
		}
	}
	h.tick(New(cfg, h.db), d0.Add(-400*time.Millisecond))
	if st := h.readState(t); st.VetoDay != "2026-11-10" || len(h.rc.received()) != before {
		t.Fatalf("veto day %q, %d requests", st.VetoDay, len(h.rc.received())-before)
	}
	os.Remove(h.cfg.PolicyPath)
	h.run(d0.Add(time.Hour), d0.AddDate(0, 0, 3))
	if after := h.sentDays(d0.AddDate(0, 0, 3))[before:]; !slices.Equal(after, []string{"2026-11-11"}) {
		t.Fatalf("sent after the veto: %v, want only 2026-11-11", after)
	}
}

// Accepted bytes whose move to sent/ is owed survive off, the
// administrator's file and a veto, are never sent again, and expire 30 days
// after their acceptance even while the move keeps failing.
func TestAcceptedBytesOutliveOptOutsAndExpire(t *testing.T) {
	for _, optOut := range []string{"off", "administrator", "veto"} {
		t.Run(optOut, func(t *testing.T) {
			h := newHall(t)
			h.setMode("on", d0.AddDate(0, 0, -2))
			h.hookActivity(d0.Add(-time.Hour))
			blocker := filepath.Join(h.state, "telemetry", "sent")
			if err := os.WriteFile(blocker, nil, 0600); err != nil {
				t.Fatal(err)
			}
			h.run(d0.Add(time.Minute), d0.Add(7*time.Hour))
			if len(h.rc.received()) != 1 || len(h.files("pending")) != 1 {
				t.Fatalf("%d requests, pending %v", len(h.rc.received()), h.files("pending"))
			}
			switch optOut {
			case "off":
				if _, _, err := telemetryconfig.SetMode(h.state, telemetryconfig.Off, "owner-command", ""); err != nil {
					t.Fatal(err)
				}
			case "administrator":
				if err := os.WriteFile(h.cfg.PolicyPath, nil, 0644); err != nil {
					t.Fatal(err)
				}
			case "veto":
				h.cfg.Vetoed = true
			}
			h.run(d0.Add(8*time.Hour), d0.AddDate(0, 0, 29))
			if len(h.rc.received()) != 1 || len(h.files("pending")) != 1 {
				t.Fatalf("after 29 days: %d requests, pending %v", len(h.rc.received()), h.files("pending"))
			}
			h.run(d0.AddDate(0, 0, 30).Add(8*time.Hour), d0.AddDate(0, 0, 31))
			if len(h.rc.received()) != 1 || len(h.files("pending")) != 0 {
				t.Fatalf("after 31 days: %d requests, pending %v", len(h.rc.received()), h.files("pending"))
			}
		})
	}
}

// An opt-out is recorded even when the mode file cannot be read at that
// moment.
func TestOptOutIsRecordedWithAnUnreadableMode(t *testing.T) {
	h := newHall(t)
	h.setMode("on", d0.AddDate(0, 0, -3))
	mode := filepath.Join(h.state, "telemetry", "mode")
	if err := errors.Join(os.WriteFile(mode, []byte("garbage"), 0600), os.WriteFile(h.cfg.PolicyPath, nil, 0644)); err != nil {
		t.Fatal(err)
	}
	h.tick(New(h.cfg, h.db), d0.Add(time.Hour))
	if st := h.readState(t); st.VetoDay != "2026-11-10" {
		t.Fatalf("veto day %q", st.VetoDay)
	}
}

// A veto observed while state.json cannot be replaced is kept by the
// running sender: nothing is sent while that day is not durable, and once
// writes work again the same sender records it first and never sends it.
func TestVetoSurvivesAFailedStateWrite(t *testing.T) {
	h := newHall(t)
	h.setMode("on", d0.AddDate(0, 0, -3))
	for _, back := range []int{-2, 0, 1} {
		h.hookActivity(d0.AddDate(0, 0, back).Add(time.Hour))
	}
	var deny atomic.Bool
	cfg := h.cfg
	cfg.failStateWrite = deny.Load
	s := New(cfg, h.db)
	h.tick(s, d0.AddDate(0, 0, -1).Add(time.Minute))
	if len(h.files("pending")) != 1 {
		t.Fatalf("pending %v", h.files("pending"))
	}
	deny.Store(true)
	if err := os.WriteFile(h.cfg.PolicyPath, nil, 0644); err != nil {
		t.Fatal(err)
	}
	h.tick(s, d0.Add(10*time.Hour))
	if st := h.readState(t); st.VetoDay != "" || len(h.files("pending")) != 0 {
		t.Fatalf("veto day %q on disk, pending %v", st.VetoDay, h.files("pending"))
	}
	if err := os.Remove(h.cfg.PolicyPath); err != nil {
		t.Fatal(err)
	}
	for now := d0.Add(11 * time.Hour); now.Before(d0.AddDate(0, 0, 2)); now = now.Add(time.Hour) {
		h.tick(s, now)
	}
	if n := len(h.rc.received()); n != 0 {
		t.Fatalf("%d requests while the opt-out day was not durable", n)
	}
	deny.Store(false)
	for now := d0.AddDate(0, 0, 2); now.Before(d0.AddDate(0, 0, 4)); now = now.Add(time.Hour) {
		h.tick(s, now)
	}
	if st, days := h.readState(t), h.sentDays(d0.AddDate(0, 0, 4)); st.VetoDay != "2026-11-10" || !slices.Equal(days, []string{"2026-11-11"}) {
		t.Fatalf("veto day %q, sent %v", st.VetoDay, days)
	}
}

// Off between the receiver taking the body and the 204 being recorded
// deletes the local copy, as off deletes every unsent report, and that day
// is never sent again.
func TestOffAroundAnAcceptanceNeverResends(t *testing.T) {
	h := newHall(t)
	h.setMode("on", d0.AddDate(0, 0, -3))
	h.hookActivity(d0.AddDate(0, 0, -2).Add(time.Hour))
	h.rc.respond = func(n int, w http.ResponseWriter, _ *http.Request) {
		if n == 0 {
			if _, _, err := telemetryconfig.SetMode(h.state, telemetryconfig.Off, "owner-command", ""); err != nil {
				t.Error(err)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}
	h.run(d0.AddDate(0, 0, -1).Add(time.Minute), d0.AddDate(0, 0, -1).Add(7*time.Hour))
	if len(h.rc.received()) != 1 || len(h.files("pending")) != 0 || len(h.files("sent")) != 0 {
		t.Fatalf("%d requests, pending %v, sent %v", len(h.rc.received()), h.files("pending"), h.files("sent"))
	}
	h.setMode("on", d0.AddDate(0, 0, -1).Add(8*time.Hour))
	h.hookActivity(d0.Add(time.Hour))
	h.hookActivity(d0.AddDate(0, 0, 1).Add(time.Hour))
	h.run(d0.AddDate(0, 0, -1).Add(9*time.Hour), d0.AddDate(0, 0, 4))
	if days := h.sentDays(d0.AddDate(0, 0, 4)); days[0] != "2026-11-08" || slices.Contains(days[1:], "2026-11-08") {
		t.Fatalf("sent %v", days)
	}
}
