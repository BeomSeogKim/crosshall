package telemetrysend

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/telemetryconfig"
)

func (h *hall) sentDays(now time.Time) []string {
	var days []string
	for _, body := range h.rc.received() {
		days = append(days, reportOf(h.t, body, now).Day)
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
	if st := h.readState(t); st.SuppressedThrough.Before(d0.Add(time.Minute)) || len(h.files("pending")) != 0 {
		t.Fatalf("suppressed through %v, pending %v", st.SuppressedThrough, h.files("pending"))
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
