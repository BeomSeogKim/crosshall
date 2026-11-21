//go:build darwin || linux

package proc

import (
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func startProbeChild(t *testing.T) *exec.Cmd {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), "/bin/sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd
}

func TestLaunchIdentityAndProcessGone(t *testing.T) {
	child := startProbeChild(t)
	wantPID := int64(child.Process.Pid)
	process := "pid-" + strconv.FormatInt(wantPID, 10)
	ref := process + "-ab"
	start, err := processStart(int(wantPID))
	if err != nil {
		t.Fatal(err)
	}
	pid, identity := LaunchIdentity(ref, start.AtMS)
	if pid != wantPID || identity <= 0 || identity != start.Identity {
		t.Fatalf("launch identity = (%d, %d), want (%d, %d)", pid, identity, wantPID, start.Identity)
	}
	for _, invalid := range []string{"", "session", "pid-0-ab", "pid-01-ab", "pid-2147483648-ab", process, process + "-"} {
		if p, i := LaunchIdentity(invalid, start.AtMS); p != 0 || i != 0 {
			t.Errorf("invalid launch reference %q returned (%d, %d)", invalid, p, i)
		}
	}
	for _, before := range []int64{0, start.AtMS - 1} {
		if p, i := LaunchIdentity(ref, before); p != 0 || i != 0 {
			t.Errorf("launch after hook time %d returned (%d, %d)", before, p, i)
		}
	}
	if ProcessGone(pid, identity) || ProcessGone(pid, 0) || !ProcessGone(pid, identity+1) {
		t.Fatal("death evidence did not distinguish the live process, a missing identity and a different process")
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Second); !ProcessGone(pid, identity); {
		if time.Now().After(deadline) {
			t.Fatal("an exited, unreaped child was not classified as gone")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := child.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("child reaped before checking its zombie state: %v", err)
	}
	if p, i := LaunchIdentity(ref, time.Now().UnixMilli()); p != 0 || i != 0 {
		t.Errorf("a zombie returned a launch identity (%d, %d)", p, i)
	}
	_ = child.Wait()
	if !ProcessGone(pid, identity) {
		t.Fatal("a reaped child was not classified as gone")
	}
}
