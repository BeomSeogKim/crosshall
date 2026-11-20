package ident

import (
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestLaunchAliveNeedsTheProcessThatStartedTheLaunch(t *testing.T) {
	self := "pid-" + strconv.Itoa(os.Getpid()) + "-0a1b"
	start, err := processStartMS(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if now := time.Now().UnixMilli(); start <= 0 || start > now {
		t.Fatalf("start %d, now %d", start, now)
	}
	if !LaunchAlive(self, time.Now().UnixMilli()) {
		t.Fatal("a running launch reads as ended")
	}
	// A launch whose first hook ran before this process started: the PID
	// was reused, however soon after.
	for _, before := range []int64{1, 500, time.Minute.Milliseconds()} {
		if LaunchAlive(self, start-before) {
			t.Fatalf("a PID reused %d ms after the launch's hook reads as the launch", before)
		}
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

func TestLaunchAliveRejectsAnExitedUnreapedProcess(t *testing.T) {
	child := exec.Command("/bin/sleep", "60")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	pid := strconv.Itoa(child.Process.Pid)
	ref, hookAt := "pid-"+pid+"-ab", time.Now().UnixMilli()
	if !LaunchAlive(ref, hookAt) {
		t.Fatal("a running process reads as ended")
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	// Until Wait reaps it, the killed child stays a zombie under its PID.
	for deadline := time.Now().Add(2 * time.Second); LaunchAlive(ref, hookAt); {
		if time.Now().After(deadline) {
			t.Fatal("a killed, unreaped process reads as running")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := child.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("the child was reaped before the check: %v", err)
	}
}
