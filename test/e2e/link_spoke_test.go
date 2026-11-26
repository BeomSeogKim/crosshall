//go:build e2e

package e2e

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/proc"
)

func TestSpokeLinkRealHome(t *testing.T) {
	f := newLinkFixture(t, binary, binary)
	home, spoke := f.home.owner(), f.spoke.owner()
	home.must("owner", "start")
	spoke.must("owner", "start")
	home.must("owner", "channel", "create", "home-only")
	spoke.must("owner", "channel", "create", "spoke-only")
	member := f.spoke.member()
	member.must("join", "spoke-only")
	if got := member.run("link", "status"); got.exit == 0 {
		t.Fatalf("member could inspect owner link status: %+v", got)
	}
	disabled := f.spoke.owner()
	disabled.extra = []string{"CROSSHALL_EXPERIMENTAL_LINK=0"}
	if got := disabled.run("link", "status"); got.exit == 0 {
		t.Fatalf("link status was enabled without opt-in: %+v", got)
	}

	link := []string{"link", linkFixtureTarget, "--name", "fixture-spoke"}
	enrollLog := f.ssh + ".enrolls"
	if got := spoke.run(link...); got.exit == 0 || !strings.Contains(got.stderr, "Link and enroll this device?") {
		t.Fatalf("link without confirmation: %+v", got)
	}
	if _, err := os.Stat(enrollLog); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("link enrolled before confirmation: %v", err)
	}

	drop := f.ssh + ".drop-enrollment"
	if err := os.WriteFile(drop, nil, 0600); err != nil {
		t.Fatal(err)
	}
	link = append(link, "--yes")
	if got := spoke.run(link...); got.exit == 0 || !strings.Contains(got.stdout, "retry the same link") {
		t.Fatalf("lost enrollment response: %+v", got)
	}
	if got := spoke.must("link", "status"); got.obj["state"] != "unlinked" {
		t.Fatalf("pending enrollment switched modes: %+v", got)
	}
	local := linkDispatcher(f.spoke)
	member.must("who")
	if err := os.Remove(drop); err != nil {
		t.Fatal(err)
	}

	spoke.must(link...)
	waitSpokeLink(t, spoke, "connected")
	keys, err := os.ReadFile(enrollLog)
	if err != nil {
		t.Fatal(err)
	}
	retries := strings.Fields(string(keys))
	if len(retries) != 2 || retries[0] != retries[1] {
		t.Fatal("enrollment retry did not reuse the first operation key")
	}
	if local.alive() {
		t.Fatal("the local dispatcher remained active after linking")
	}
	if data, err := os.Stat(filepath.Join(f.spoke.state, "link.json")); err != nil || data.Mode().Perm() != 0600 {
		t.Fatalf("link profile is missing or not private: %v, %v", data, err)
	}
	if got := member.run("who"); got.obj["code"] != "unsent" ||
		!strings.Contains(got.stdout, "ask the owner to run crosshall link status") {
		t.Fatalf("member command was not refused with an owner-facing hint: %+v", got)
	}
	if got := spoke.run("owner", "channel", "create", "wrong-device"); got.obj["code"] != "owner_only" {
		t.Fatalf("owner command entered local mode while linked: %+v", got)
	}
	home.must("owner", "who", "--all")

	first := killSpokeSSH(t, f.ssh)
	waitSpokeLink(t, spoke, "disconnected")
	waitSpokeLink(t, spoke, "connected")
	flapAt := time.Now()
	second := killSpokeSSH(t, f.ssh)
	if second == first {
		t.Fatal("the spoke did not start another SSH child")
	}
	waitSpokeLink(t, spoke, "disconnected")
	waitSpokeLink(t, spoke, "connected")
	if delay := time.Since(flapAt); delay < 2*time.Second {
		t.Fatalf("the short-lived connection reset reconnect backoff: %v", delay)
	}
	next := spokeLinkSSHPID(t, f.ssh)
	if next == second {
		t.Fatal("the spoke did not start another SSH child")
	}
	reconnected, err := proc.StartOf(next)
	if err != nil {
		t.Fatal(err)
	}

	spoke.must("unlink")
	waitSpokeLink(t, spoke, "unlinked")
	if proc.ProcessAlive(int64(next), reconnected.Identity) {
		t.Fatal("unlink left its SSH child active")
	}
	member.must("who")
	if got := f.spoke.member().run("join", "wrong-device"); got.obj["code"] != "not_found" {
		t.Fatalf("a spoke owner command changed the preserved local hall: %+v", got)
	}
}

func waitSpokeLink(t *testing.T, spoke *participant, state string) {
	t.Helper()
	for deadline := time.Now().Add(fixtureEventTimeout); ; time.Sleep(30 * time.Millisecond) {
		got := spoke.must("link", "status")
		if got.obj["state"] == state {
			if state == "connected" && (got.obj["target"] != linkFixtureTarget ||
				got.obj["home_release"] == "" || got.obj["link_version"] != float64(1) || got.obj["clock_offset_ms"] == nil) {
				t.Fatalf("incomplete negotiated link status: %+v", got)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("waiting for %s link status: %+v", state, got)
		}
	}
}

func spokeLinkSSHPID(t *testing.T, ssh string) int {
	t.Helper()
	data, err := os.ReadFile(ssh + ".pid")
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		t.Fatalf("invalid SSH fixture PID: %q", data)
	}
	return pid
}

func killSpokeSSH(t *testing.T, ssh string) int {
	t.Helper()
	pid := spokeLinkSSHPID(t, ssh)
	start, err := proc.StartOf(pid)
	if err != nil || !proc.ProcessAlive(int64(pid), start.Identity) {
		t.Fatalf("fixture SSH child %d is not active: %v", pid, err)
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	return pid
}
