//go:build e2e

package e2e

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

const silentInstance = "silent-fixture"

// holdSilentDispatcher leaves plausible ready files and a held state lock
// without a daemon that can answer a request.
func holdSilentDispatcher(t *testing.T, w *world) func() {
	t.Helper()
	for _, dir := range []string{w.state, w.rendezvous()} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	lock, err := os.OpenFile(filepath.Join(w.state, "dispatcher.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lock.Close() })
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	root, err := spool.OpenServerRoot(w.rendezvous())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	lease, err := spool.HoldReadyLease(root, silentInstance)
	if err != nil {
		t.Fatal(err)
	}
	release := func() {
		lease.Close()
		lock.Close()
	}
	t.Cleanup(release)
	if err := spool.PublishServerFile(root, "ready", map[string]any{
		"v": wire.Version, "pid": os.Getpid(), "instance": silentInstance, "lease": true, "probe": true,
	}); err != nil {
		t.Fatal(err)
	}
	return release
}

func TestOwnerStartTimesOutWhenIncumbentIsSilent(t *testing.T) {
	w := newWorld(t)
	holdSilentDispatcher(t, w)
	got := w.owner().run("owner", "start")
	if got.exit != 1 || !strings.Contains(got.stdout, "dispatcher did not become ready") ||
		!strings.Contains(got.stdout, "context deadline exceeded") {
		t.Fatalf("owner start must time out with the lock held and no reply: %+v", got)
	}
}

func TestOwnerStartWaitsForSilentIncumbentToReleaseLock(t *testing.T) {
	w := newWorld(t)
	release := holdSilentDispatcher(t, w)

	cmd := exec.CommandContext(t.Context(), binary, "--json", "owner", "start")
	cmd.Dir, cmd.Env = w.home, w.owner().env()
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timeout := time.NewTimer(3 * time.Second)
	defer timeout.Stop()
	poll := time.NewTicker(5 * time.Millisecond)
	defer poll.Stop()

	// Keep the incumbent locked until the launcher tries to contact it.
	in := filepath.Join(w.state, "owner", "in")
waitForProbe:
	for {
		select {
		case err := <-done:
			t.Fatalf("owner start returned with the silent incumbent still locked: %v, %s", err, output.String())
		case <-timeout.C:
			t.Fatal("owner start sent no readiness probe")
		case <-poll.C:
			files, _ := os.ReadDir(in)
			for _, file := range files {
				if strings.HasSuffix(file.Name(), ".json") {
					break waitForProbe
				}
			}
		}
	}
	release()
	select {
	case err := <-done:
		var response struct{ OK bool }
		if err != nil || json.Unmarshal(output.Bytes(), &response) != nil || !response.OK {
			t.Fatalf("replacement did not become ready: %v, %s", err, output.String())
		}
	case <-time.After(4 * time.Second):
		t.Fatal("owner start did not return after the incumbent released its lock")
	}

	raw, err := os.ReadFile(filepath.Join(w.rendezvous(), "ready"))
	var marker struct{ Instance string }
	if err != nil || json.Unmarshal(raw, &marker) != nil || marker.Instance == "" || marker.Instance == silentInstance {
		t.Fatalf("replacement did not publish a new ready instance: %s, %v", raw, err)
	}
	assertReadyLease(t, w.rendezvous(), marker.Instance, true)
}
