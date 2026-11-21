//go:build e2e

package e2e

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestHookEntryRejectsReleasedLeaseWithReusedPID(t *testing.T) {
	for _, tc := range []struct {
		event, payload string
		member         bool
	}{
		{"UserPromptSubmit", hookPrompt, true},
		{"SessionStart", `{"hook_event_name":"SessionStart","session_id":"synthetic-hook-member","source":"startup"}`, false},
	} {
		t.Run(tc.event, func(t *testing.T) {
			h := newHookWorld(t, tc.member)
			// Model PID reuse with this still-running process and the old,
			// unlocked lease left in the rendezvous by the dispatcher.
			h.write(filepath.Join(h.rendezvous(), "ready"), fmt.Sprintf(
				`{"v":1,"instance":"fixture-instance","pid":%d,"lease":true}`, os.Getpid()))
			h.write(filepath.Join(h.rendezvous(), "ready.lease"), "fixture-instance")
			if out := h.run(strings.NewReader(tc.payload), nil, "codex", tc.event); out != "" {
				t.Fatalf("unexpected hook output %q", out)
			}
			h.assertCounter(tc.event, "no_dispatcher")
			h.assertSpoolsEmpty()
			if _, err := os.Stat(filepath.Join(h.leaf, "hook-publish-seq")); !os.IsNotExist(err) {
				t.Fatalf("stale dispatcher received a hook publication: %v", err)
			}
		})
	}
}

func TestHookEntryRejectsLeaseForDifferentInstance(t *testing.T) {
	h := newHookWorld(t, true)
	h.write(filepath.Join(h.rendezvous(), "ready"), fmt.Sprintf(
		`{"v":1,"instance":"old-instance","pid":%d,"lease":true}`, os.Getpid()))
	path := filepath.Join(h.rendezvous(), "ready.lease")
	h.write(path, "new-instance")
	lease, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	if err := syscall.Flock(int(lease.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	if out := h.run(strings.NewReader(hookPrompt), nil, "codex", "UserPromptSubmit"); out != "" {
		t.Fatalf("unexpected hook output %q", out)
	}
	h.assertCounter("UserPromptSubmit", "no_dispatcher")
	h.assertSpoolsEmpty()
}

func TestDispatcherRepublishesReadyLeaseAfterPurge(t *testing.T) {
	w := newWorld(t)
	w.owner().must("owner", "start")
	rv := w.rendezvous()
	readInstance := func() string {
		raw, err := os.ReadFile(filepath.Join(rv, "ready"))
		if errors.Is(err, os.ErrNotExist) {
			return ""
		}
		var ready struct {
			Instance string `json:"instance"`
			Lease    bool   `json:"lease"`
		}
		if err != nil || json.Unmarshal(raw, &ready) != nil || !ready.Lease || ready.Instance == "" {
			t.Fatalf("invalid leased readiness marker %q: %v", raw, err)
		}
		return ready.Instance
	}
	instance := readInstance()
	assertReadyLease(t, rv, instance, true)

	retired := rv + ".retired"
	if err := os.Rename(rv, retired); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(retired) })
	deadline := time.Now().Add(fixtureEventTimeout)
	for {
		if current := readInstance(); current != "" {
			if current != instance {
				t.Fatalf("purge replaced dispatcher instance: %q -> %q", instance, current)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("dispatcher did not restore readiness after purge")
		}
		time.Sleep(10 * time.Millisecond)
	}
	assertReadyLease(t, rv, instance, true)
	assertReadyLease(t, retired, instance, false)
	w.kill()
	assertReadyLease(t, rv, instance, false)
}

func assertReadyLease(t *testing.T, path, instance string, held bool) {
	t.Helper()
	lease, err := os.Open(filepath.Join(path, "ready.lease"))
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close()
	lockErr := syscall.Flock(int(lease.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
	if held && lockErr != syscall.EWOULDBLOCK || !held && lockErr != nil {
		t.Fatalf("lease held = %t: %v", held, lockErr)
	}
	contents, err := os.ReadFile(lease.Name())
	if err != nil || string(contents) != instance {
		t.Fatalf("lease has wrong instance %q: %v", contents, err)
	}
}
