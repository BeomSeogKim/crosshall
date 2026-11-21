//go:build darwin || linux

package proc

import (
	"os/exec"
	"runtime"
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

func TestProbeLaunchAndProcessGone(t *testing.T) {
	child := startProbeChild(t)
	wantPID := int64(child.Process.Pid)
	process := "pid-" + strconv.FormatInt(wantPID, 10)
	ref := process + "-ab"
	start, err := processStart(int(wantPID))
	if err != nil {
		t.Fatal(err)
	}
	pid, identity, gone := ProbeLaunch(ref, start.AtMS)
	if pid != wantPID || identity <= 0 || identity != start.Identity || gone {
		t.Fatalf("live launch probe = (%d, %d, %t), want (%d, %d, false)", pid, identity, gone, wantPID, start.Identity)
	}
	for _, invalid := range []string{"", "session", "pid-0-ab", "pid-01-ab", "pid-2147483648-ab", process, process + "-"} {
		if p, i, gone := ProbeLaunch(invalid, start.AtMS); p != 0 || i != 0 || gone {
			t.Errorf("invalid launch reference %q probed as (%d, %d, %t)", invalid, p, i, gone)
		}
	}
	if p, i, gone := ProbeLaunch(ref, 0); p != 0 || i != 0 || gone {
		t.Errorf("live process with unknown hook time probed as (%d, %d, %t)", p, i, gone)
	}
	wantLaterPID, wantLaterGone := int64(0), runtime.GOOS == "darwin"
	if wantLaterGone {
		wantLaterPID = pid
	}
	if p, i, gone := ProbeLaunch(ref, start.AtMS-1); p != wantLaterPID || i != 0 || gone != wantLaterGone {
		t.Errorf("process with start after hook probed as (%d, %d, %t), want (%d, 0, %t)", p, i, gone, wantLaterPID, wantLaterGone)
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
	for _, hookAt := range []int64{0, start.AtMS} {
		if p, i, gone := ProbeLaunch(ref, hookAt); p != pid || i != 0 || !gone {
			t.Errorf("a zombie with hook time %d probed as (%d, %d, %t)", hookAt, p, i, gone)
		}
	}
	_ = child.Wait()
	if !ProcessGone(pid, identity) {
		t.Fatal("a reaped child was not classified as gone")
	}
	for _, hookAt := range []int64{0, start.AtMS} {
		if p, i, gone := ProbeLaunch(ref, hookAt); p != pid || i != 0 || !gone {
			t.Errorf("a reaped child with hook time %d probed as (%d, %d, %t)", hookAt, p, i, gone)
		}
	}
}
