package ident

import (
	"os"
	"strconv"
	"testing"
	"time"
)

func TestLaunchAliveNeedsTheProcessThatStartedTheLaunch(t *testing.T) {
	self := "pid-" + strconv.Itoa(os.Getpid()) + "-0a1b"
	start, err := processStartMS(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if now := time.Now().UnixMilli(); start <= 0 || start > now+launchClockSlackMS {
		t.Fatalf("start %d, now %d", start, now)
	}
	if !LaunchAlive(self, time.Now().UnixMilli()) {
		t.Fatal("a running launch reads as ended")
	}
	// A launch whose first hook ran before this process started: the PID
	// was reused.
	if LaunchAlive(self, start-launchClockSlackMS-time.Minute.Milliseconds()) {
		t.Fatal("a reused PID reads as the launch")
	}
	for _, ref := range []string{"session", "", strconv.Itoa(os.Getpid()), "pid-0-ab", "pid--1-ab", "pid-01-ab",
		"pid-" + strconv.Itoa(1<<22) + "-ab"} {
		if LaunchAlive(ref, time.Now().UnixMilli()) {
			t.Fatalf("launch %q reads as alive", ref)
		}
	}
}

func TestLaunchProcessGroupsLaunchesOfOneProcess(t *testing.T) {
	for ref, want := range map[string]string{"pid-42-aa": "pid-42", "pid-42-bb": "pid-42", "pid-42": "pid-42",
		"pid-7-": "pid-7", "session": "", "pid-x-aa": "", "unbound-1": ""} {
		if got, ok := LaunchProcess(ref); got != want || ok != (want != "") {
			t.Fatalf("%q: %q, %v", ref, got, ok)
		}
	}
}
