package dispatch

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/hook"
	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
	"github.com/BeomSeogKim/crosshall/internal/setup"
)

// withHostBuildAdapter serves the Antigravity manifest with
// build_from_host, whatever the shipped revision declares.
func withHostBuildAdapter(t *testing.T) {
	t.Helper()
	raw, err := manifests.FS.ReadFile("agy.json")
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["build_from_host"] = true
	edited, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(manifests.SwapReader(func(adapter string) ([]byte, error) {
		if adapter == "agy" {
			return edited, nil
		}
		return manifests.FS.ReadFile(adapter + ".json")
	}))
}

func TestTheDispatcherPublishesOnlyCurrentRecordedBuildsAndRereadsTheRecordedPath(t *testing.T) {
	withHostBuildAdapter(t)
	dir := t.TempDir()
	stateDir, rvDir := filepath.Join(dir, "state"), filepath.Join(dir, "rv")
	for _, d := range []string{stateDir, rvDir} {
		if err := os.Mkdir(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	exe := filepath.Join(dir, "agy")
	write := func(version string, at time.Time) {
		if err := os.WriteFile(exe, []byte("#!/bin/sh\necho "+version+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(exe, at, at); err != nil {
			t.Fatal(err)
		}
	}
	write("1.5.16", time.Now().Add(-time.Hour))
	recorded, ok := setup.Reprobe(t.Context(), "agy", setup.HostBuild{Executable: exe}, 2*time.Second)
	if !ok {
		t.Fatal("no recorded build")
	}
	saved := recordedHostBuild
	recordedHostBuild = func(string, string) (setup.HostBuild, bool) { return recorded, true }
	t.Cleanup(func() { recordedHostBuild = saved })
	state, err := os.OpenRoot(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()

	line := hook.HostBuildLine("agy", "1.5.16", recorded.Identity, recorded.Size, recorded.ModifiedNS, exe)
	if got := hostBuildText(stateDir, state); got != line {
		t.Fatalf("recorded build: %q, want %q", got, line)
	}
	// A refresh of another path never stands for the recorded one.
	other := recorded
	other.Executable, other.Version = exe+"-elsewhere", "9.9.9"
	data, _ := json.Marshal(map[string]setup.HostBuild{"agy": other})
	if err := publishAdapterText(state, hostBuildsRefreshed, string(data)); err != nil {
		t.Fatal(err)
	}
	if got := hostBuildText(stateDir, state); got != line {
		t.Fatalf("with a refresh of another path: %q", got)
	}

	write("1.5.17 ", time.Now())
	if got := hostBuildText(stateDir, state); got != "" {
		t.Fatalf("a changed executable is still published: %q", got)
	}
	if _, recorded, current := effectiveHostBuild(stateDir, state, "agy"); !recorded || current {
		t.Fatal("doctor would not name the changed executable")
	}
	s := &server{cfg: Config{StateRoot: stateDir, Rendezvous: rvDir}, state: state}
	s.hostBuildTick(t.Context(), time.Now())
	s.builds.workers.Wait()
	s.builds.next = time.Time{}
	s.hostBuildTick(t.Context(), time.Now())
	s.builds.workers.Wait()
	published, err := os.ReadFile(filepath.Join(rvDir, hook.HostBuildsName))
	if err != nil || !strings.HasPrefix(string(published), "agy 1.5.17 ") || !strings.HasSuffix(string(published), " "+exe+"\n") {
		t.Fatalf("after the refresh: %q (%v)", published, err)
	}
	if _, _, current := effectiveHostBuild(stateDir, state, "agy"); !current {
		t.Fatal("the refreshed build is not current")
	}
}

// The refresh worker logs its own failures: the main loop's failure log is
// not shared with it. Run with -race.
func TestTheRefreshWorkerSharesNoStateWithTheMainLoop(t *testing.T) {
	withHostBuildAdapter(t)
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	if err := os.Mkdir(stateDir, 0700); err != nil {
		t.Fatal(err)
	}
	// A directory where the refreshed builds go makes the worker's write fail.
	if err := os.Mkdir(filepath.Join(stateDir, hostBuildsRefreshed), 0700); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "agy")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho 1.5.16\n"), 0700); err != nil {
		t.Fatal(err)
	}
	recorded, ok := setup.Reprobe(t.Context(), "agy", setup.HostBuild{Executable: exe}, 2*time.Second)
	if !ok {
		t.Fatal("no recorded build")
	}
	recorded.Size++ // stale: the worker reads the version again
	saved := recordedHostBuild
	recordedHostBuild = func(string, string) (setup.HostBuild, bool) { return recorded, true }
	t.Cleanup(func() { recordedHostBuild = saved })
	state, err := os.OpenRoot(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	s := &server{cfg: Config{StateRoot: stateDir, Rendezvous: filepath.Join(dir, "rv")}, state: state}
	s.hostBuildTick(t.Context(), time.Now())
	for range 50 {
		s.logFailure(errors.New("main loop failure"))
	}
	s.builds.workers.Wait()
}
