package telemetryconfig

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTelemetryFirstAnswerWins(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	var writes atomic.Int32
	var wait sync.WaitGroup
	for _, mode := range []string{On, Off} {
		wait.Go(func() {
			_, changed, err := SetMode(root, mode, "owner-prompt", Unset)
			if err != nil {
				t.Error(err)
			}
			if changed {
				writes.Add(1)
			}
		})
	}
	wait.Wait()
	if writes.Load() != 1 {
		t.Fatalf("expected one recorded initial answer, got %d", writes.Load())
	}
	setting, err := ReadMode(root)
	if err != nil || (setting.Mode != On && setting.Mode != Off) ||
		setting.ChangedAt.IsZero() || setting.Source != "owner-prompt" {
		t.Fatalf("saved first answer: %+v, error=%v", setting, err)
	}
}

func TestTelemetryOffDiscardsPendingAndPreservesSent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	if _, _, err := SetMode(root, On, "owner-command", ""); err != nil {
		t.Fatal(err)
	}
	pending := filepath.Join(root, "telemetry", "pending")
	sent := filepath.Join(root, "telemetry", "sent")
	for _, dir := range []string{pending, sent} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "2026-11-20.json"), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// A batch the receiver accepted, whose move to sent/ is owed, keeps
	// its bytes.
	if err := os.WriteFile(filepath.Join(pending, "2026-11-19.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	dir, err := os.OpenRoot(filepath.Join(root, "telemetry"))
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if err := WriteState(dir, &State{Days: map[string]*DayState{"2026-11-19": {Batch: true, Accepted: true}}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := SetMode(root, Off, "owner-command", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(pending, "2026-11-20.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending telemetry report survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(pending, "2026-11-19.json")); err != nil {
		t.Fatalf("accepted report was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(sent, "2026-11-20.json")); err != nil {
		t.Fatal(err)
	}
}

func TestTelemetryEligibilityStartsAtTheNextUTCMidnight(t *testing.T) {
	answer := time.Date(2026, 11, 24, 8, 59, 0, 0, time.FixedZone("UTC+9", 9*60*60))
	if got := NextFullDay(answer).Format(time.RFC3339); got != "2026-11-24T00:00:00Z" {
		t.Fatalf("first complete UTC day: %s", got)
	}
}

func TestTelemetryExplicitOffPrevailsOverFirstAnswer(t *testing.T) {
	for _, order := range []string{"off first", "answer first", "concurrent"} {
		t.Run(order, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "state")
			off := func() {
				if _, _, err := SetMode(root, Off, "owner-command", ""); err != nil {
					t.Error(err)
				}
			}
			answer := func() {
				if _, _, err := SetMode(root, On, "owner-prompt", Unset); err != nil {
					t.Error(err)
				}
			}
			switch order {
			case "off first":
				off()
				answer()
			case "answer first":
				answer()
				off()
			case "concurrent":
				var wait sync.WaitGroup
				start := make(chan struct{})
				wait.Go(func() { <-start; answer() })
				wait.Go(func() { <-start; off() })
				close(start)
				wait.Wait()
			}
			setting, err := ReadMode(root)
			if err != nil || setting.Mode != Off || setting.Source != "owner-command" {
				t.Fatalf("off should prevail: setting=%+v, error=%v", setting, err)
			}
		})
	}
}

func TestRepeatedTelemetryOnPreservesContinuousConsent(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	first := time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)
	setting, changed, err := setMode(root, On, "owner-prompt", Unset, func() time.Time { return first })
	if err != nil || !changed {
		t.Fatalf("initial choice: %+v changed=%v err=%v", setting, changed, err)
	}
	repeated, changed, err := setMode(root, On, "owner-command", "", func() time.Time {
		return first.AddDate(0, 0, 5)
	})
	if err != nil || changed || repeated != setting {
		t.Fatalf("repeated on: %+v changed=%v err=%v; initial was %+v", repeated, changed, err, setting)
	}
	got, err := ReadMode(root)
	if err != nil || got != setting || NextFullDay(got.ChangedAt).Format(time.DateOnly) != "2026-11-19" {
		t.Fatalf("continuous consent: %+v, %v", got, err)
	}
}

func TestTelemetryTransitionsKeepTheOffDayIneligible(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	beforeMidnight := time.Date(2026, 11, 23, 23, 59, 59, 0, time.UTC)
	afterMidnight := beforeMidnight.Add(2 * time.Second)
	sampled := make(chan struct{})
	resume := make(chan struct{})
	onDone, offDone := make(chan error, 1), make(chan error, 1)
	go func() {
		_, _, err := setMode(root, On, "owner-command", "", func() time.Time {
			close(sampled)
			<-resume
			return beforeMidnight
		})
		onDone <- err
	}()
	<-sampled
	go func() {
		_, _, err := setMode(root, Off, "owner-command", "", func() time.Time { return afterMidnight })
		offDone <- err
	}()
	select {
	case err := <-offDone:
		t.Errorf("off completed while an older decision could still replace it: %v", err)
		close(resume)
		<-onDone
		return
	case <-time.After(40 * time.Millisecond):
		close(resume)
	}
	if err := <-onDone; err != nil {
		t.Fatal(err)
	}
	if err := <-offDone; err != nil {
		t.Fatal(err)
	}
	off, err := ReadMode(root)
	if err != nil || off.Mode != Off || !off.ChangedAt.Equal(afterMidnight) {
		t.Fatalf("newer off was not preserved: %+v, %v", off, err)
	}
	on, changed, err := setMode(root, On, "owner-command", "", func() time.Time {
		return beforeMidnight
	})
	if err != nil || !changed || NextFullDay(on.ChangedAt).Format(time.DateOnly) != "2026-11-25" {
		t.Fatalf("a clock rollback made the off day eligible: %+v, changed=%v, %v", on, changed, err)
	}
}

func TestDoNotTrackNonEmptyVeto(t *testing.T) {
	t.Setenv(TelemetryEnv, "")
	for _, tc := range []struct {
		value string
		off   bool
	}{
		{"", false}, {"0", false}, {"false", false}, {"FALSE", false},
		{" ", true}, {" false ", true}, {"1", true},
	} {
		t.Setenv(DoNotTrackEnv, tc.value)
		if got := EnvironmentOff(); got != tc.off {
			t.Errorf("DO_NOT_TRACK=%q gives veto %v, want %v", tc.value, got, tc.off)
		}
	}
}
