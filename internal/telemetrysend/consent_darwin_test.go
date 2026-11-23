package telemetrysend

import (
	"net/http"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A batch older than seven days is never sent, even when its file cannot
// be deleted. It uses darwin's immutable file flag; on linux that flag
// needs root.
func TestExpiredBatchIsNotSentWhenItCannotBeDeleted(t *testing.T) {
	h := newHall(t)
	h.setMode("on", d0.AddDate(0, 0, -9))
	h.hookActivity(d0.AddDate(0, 0, -7).Add(time.Hour))
	h.rc.respond = func(_ int, w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }
	h.run(d0.AddDate(0, 0, -6).Add(time.Minute), d0.AddDate(0, 0, -6).Add(7*time.Hour))
	path := filepath.Join(h.state, "telemetry", "pending", "2026-11-03.json")
	if err := syscall.Chflags(path, 0x2); err != nil { // UF_IMMUTABLE
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Chflags(path, 0) })
	h.run(d0.AddDate(0, 0, -6).Add(8*time.Hour), d0.Add(23*time.Hour))
	before := len(h.rc.received())
	h.rc.respond = nil
	h.run(d0.AddDate(0, 0, 1), d0.AddDate(0, 0, 3))
	if after := h.sentDays(d0.AddDate(0, 0, 3))[before:]; len(after) != 0 || len(h.files("pending")) != 1 {
		t.Fatalf("sent after expiry: %v, pending %v", after, h.files("pending"))
	}
}
