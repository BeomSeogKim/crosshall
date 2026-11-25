package dispatch

import (
	"bytes"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/hook"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"golang.org/x/sys/unix"
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

func TestHourlyPassRenewsLaunchMarksOnlyForTheirLiveProcesses(t *testing.T) {
	s, root := adapterFilesFixture(t)
	if err := prepareNamedSpool(s.state, "owner"); err != nil {
		t.Fatal(err)
	}
	defer s.withdrawReady()
	if err := s.scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	launches, err := spool.OpenServerChild(root, "launches", true)
	if err != nil {
		t.Fatal(err)
	}
	defer launches.Close()
	dead := exec.Command("/bin/sh", "-c", "exit 0")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	reused := exec.Command("/bin/sleep", "30")
	if err := reused.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = reused.Process.Kill()
		_ = reused.Wait()
	})
	old := time.Now().Add(-5 * 24 * time.Hour)
	livePID := strconv.Itoa(os.Getpid())
	marks := map[string]time.Time{
		livePID:                          time.Now(),
		strconv.Itoa(dead.Process.Pid):   old,
		strconv.Itoa(reused.Process.Pid): old,
		livePID + "-other":               time.Now(),
	}
	before := make(map[string]unix.Stat_t, len(marks))
	for name, markedAt := range marks {
		if err := launches.WriteFile(name, nil, 0600); err != nil {
			t.Fatal(err)
		}
		if err := launches.Chtimes(name, old, markedAt); err != nil {
			t.Fatal(err)
		}
		before[name] = fileTimes(t, launches, name)
	}
	if err := s.scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	for name, previous := range before {
		if got := fileTimes(t, launches, name); got.Ctim != previous.Ctim {
			t.Errorf("launch mark %s renewed before the hour", name)
		}
	}
	s.nextReap = time.Time{}
	if err := s.scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	for name, previous := range before {
		got := fileTimes(t, launches, name)
		if renewed := got.Ctim != previous.Ctim; renewed != (name == livePID) {
			t.Errorf("launch mark %s renewed = %t, want %t", name, renewed, name == livePID)
		}
		if got.Mtim != previous.Mtim || got.Ino != previous.Ino {
			t.Errorf("launch mark %s lost its original timestamp or inode", name)
		}
	}
}

func TestHourlyPassKeepsQuietMemberReceiptsAndFastPaths(t *testing.T) {
	s, root := adapterFilesFixture(t)
	if err := prepareNamedSpool(s.state, "owner"); err != nil {
		t.Fatal(err)
	}
	defer s.withdrawReady()
	leaf := adapterLeafFixture(t, s, root, true)
	if err := s.scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"gen": "test-gen\n", "ack": "5\n", "ack-move": "2 2 5\n",
		"rules": "5\n", "rules-move": "2 2 5\n", "stamp": "stamp\n",
		hook.TurnEndAskFile: "test-dispatcher test-gen 0\n", "seen-notice-receipts": "notice\n",
		"seen-join-hint": "hint receipt\n", "join-hint-offer": "hint offer\n",
		"seen-reach": "reach receipt\n", "reach-offer": "reach offer\n",
		"hook-publish-seq": "1\n",
	}
	old := time.Now().Add(-5 * 24 * time.Hour)
	before := make(map[string]unix.Stat_t, len(files))
	for name, contents := range files {
		if err := leaf.WriteFile(name, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		if err := leaf.Chtimes(name, old, old); err != nil {
			t.Fatal(err)
		}
		before[name] = fileTimes(t, leaf, name)
	}
	if err := s.scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	for name, previous := range before {
		if got := fileTimes(t, leaf, name); got.Ctim != previous.Ctim {
			t.Errorf("member file %s renewed before the hour", name)
		}
	}
	s.nextReap = time.Time{}
	if err := s.scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	for name, contents := range files {
		gotTimes := fileTimes(t, leaf, name)
		if renewed := gotTimes.Ctim != before[name].Ctim; renewed != (name != "hook-publish-seq") {
			t.Errorf("member file %s renewed = %t", name, renewed)
		}
		if got, err := leaf.ReadFile(name); err != nil || string(got) != contents {
			t.Errorf("member file %s after renewal: %q, %v", name, got, err)
		}
	}
	if _, err := leaf.Lstat("seen-notice"); !os.IsNotExist(err) {
		t.Fatalf("renewal of a missing seen-notice: %v", err)
	}
}

func fileTimes(t *testing.T, dir *os.Root, name string) unix.Stat_t {
	t.Helper()
	var stat unix.Stat_t
	if err := unix.Lstat(filepath.Join(dir.Name(), name), &stat); err != nil {
		t.Fatal(err)
	}
	return stat
}
