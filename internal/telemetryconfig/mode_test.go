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
	if _, _, err := SetMode(root, Off, "owner-command", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pending); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending telemetry archive survived: %v", err)
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
