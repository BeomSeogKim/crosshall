package dispatch

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/hook"
	"github.com/BeomSeogKim/crosshall/internal/spool"
)

// A snapshot removed while readiness stands is republished by the next
// scan, unchanged, with one log line; ready is not rewritten (#378).
func TestScanRepublishesARemovedAdapterFileOnce(t *testing.T) {
	s, _ := adapterFilesFixture(t)
	if err := prepareNamedSpool(s.state, "owner"); err != nil {
		t.Fatal(err)
	}
	s.cfg.StateRoot = s.state.Name()
	defer s.withdrawReady()
	if err := s.scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	ready, err := os.Stat(filepath.Join(s.cfg.Rendezvous, "ready"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{adapterPolicyName, adapterRepoSetName, adapterTurnEndName, hook.HostBuildsName} {
		path := filepath.Join(s.cfg.Rendezvous, name)
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		var logged bytes.Buffer
		previous := log.Writer()
		log.SetOutput(&logged)
		for range 3 {
			if err := s.scan(t.Context()); err != nil {
				log.SetOutput(previous)
				t.Fatal(err)
			}
		}
		log.SetOutput(previous)
		if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%s after the scans: %q, %v; want %q", name, got, err, want)
		}
		lines := strings.Split(strings.TrimSpace(logged.String()), "\n")
		if len(lines) != 1 || !strings.HasSuffix(lines[0], "republished the removed "+path) {
			t.Fatalf("three scans after removing %s logged %q, want one line naming it", name, logged.String())
		}
	}
	if now, err := os.Stat(filepath.Join(s.cfg.Rendezvous, "ready")); err != nil || !os.SameFile(ready, now) {
		t.Fatalf("ready was rewritten: %v", err)
	}
}

// The hourly pass renews the times of the files hooks read, so that macOS's
// /tmp cleaner, which takes a file only when all its times are more than
// three days old, never takes them from a running dispatcher (#378).
func TestTheHourlyPassRenewsTheRendezvousFiles(t *testing.T) {
	s, _ := adapterFilesFixture(t)
	if err := prepareNamedSpool(s.state, "owner"); err != nil {
		t.Fatal(err)
	}
	s.cfg.StateRoot = s.state.Name()
	defer s.withdrawReady()
	if err := s.scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	names := []string{"ready", spool.ReadyLeaseFile, adapterPolicyName, adapterRepoSetName, adapterTurnEndName, hook.HostBuildsName}
	old := time.Now().Add(-5 * 24 * time.Hour)
	for _, name := range names {
		if err := os.Chtimes(filepath.Join(s.cfg.Rendezvous, name), old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		if fi, err := os.Stat(filepath.Join(s.cfg.Rendezvous, name)); err != nil || !fi.ModTime().Equal(old) {
			t.Fatalf("%s renewed before the hour: %v", name, err)
		}
	}
	s.nextReap = time.Time{}
	if err := s.scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		fi, err := os.Stat(filepath.Join(s.cfg.Rendezvous, name))
		if err != nil || time.Since(fi.ModTime()) > time.Minute {
			t.Fatalf("%s after the hourly pass: %v, %v", name, fi.ModTime(), err)
		}
	}
}
