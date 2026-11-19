//go:build e2e

package e2e

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func TestOwnerStopWithNoDispatcher(t *testing.T) {
	w := newWorld(t)
	got := w.owner().must("owner", "stop")
	if got.obj["stopped"] != false {
		t.Fatalf("owner stop did not report an idle dispatcher: %s", got.stdout)
	}
	for _, path := range []string{w.state, w.rendezvous()} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("owner stop with no dispatcher created %s: %v", path, err)
		}
	}
}

func TestOwnerStopWaitsAndLeavesAutomaticStartupAvailable(t *testing.T) {
	w, owner, members := setup(t)
	doctor := owner.must("owner", "doctor")
	if doctor.obj["state_dir"] != w.state || doctor.obj["rendezvous"] != w.rendezvous() {
		t.Fatalf("doctor did not report both local paths: %s", doctor.stdout)
	}
	plain := owner.mustPlain("owner", "doctor")
	if !strings.Contains(plain, "State directory: "+w.state) || !strings.Contains(plain, "Rendezvous directory: "+w.rendezvous()) {
		t.Fatalf("plain doctor did not report both local paths: %s", plain)
	}
	ready := filepath.Join(w.rendezvous(), "ready")
	instance := func() string {
		t.Helper()
		data, err := os.ReadFile(ready)
		var marker struct{ Instance string }
		if err != nil || json.Unmarshal(data, &marker) != nil || marker.Instance == "" {
			t.Fatalf("invalid readiness marker: %s, %v", data, err)
		}
		return marker.Instance
	}
	stop := func() {
		t.Helper()
		if got := owner.must("owner", "stop"); got.obj["stopped"] != true {
			t.Fatalf("owner stop did not report shutdown: %s", got.stdout)
		}
		if _, err := os.Stat(ready); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("ready marker survived owner stop: %v", err)
		}
		lock, err := os.Open(filepath.Join(w.state, "dispatcher.lock"))
		if err != nil {
			t.Fatal(err)
		}
		defer lock.Close()
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
			t.Fatalf("dispatcher retained its lock after owner stop: %v", err)
		}
	}

	old := instance()
	if got := members[0].run("owner", "stop"); got.exit != 3 || got.obj["code"] != "owner_only" {
		t.Fatalf("member could issue owner stop: %+v", got)
	}
	if instance() != old {
		t.Fatal("member stop changed the dispatcher")
	}
	stop()
	owner.must("owner", "start")
	fresh := instance()
	if fresh == old {
		t.Fatal("immediate start reused the stopped dispatcher")
	}
	assertReadyLease(t, w.rendezvous(), fresh, true)
	stop()
	if got := members[0].must("who"); len(got.aliases()) != 3 {
		t.Fatalf("member restart lost its channel: %s", got.stdout)
	}
	restarted := instance()
	if restarted == fresh {
		t.Fatal("member reused the stopped dispatcher")
	}
	assertReadyLease(t, w.rendezvous(), restarted, true)
}
