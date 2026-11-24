package testreap

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "__dispatch" {
		// A fixture dispatcher that never stops by itself.
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(Check(m.Run()))
}

// startFake runs this binary as a dispatcher of state root root and returns
// once it can be listed.
func startFake(t *testing.T, root string) int {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "__dispatch")
	cmd.Env = append(os.Environ(), stateEnv+root)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go cmd.Wait()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		found, err := owned([]string{root})
		if err != nil {
			t.Fatal(err)
		}
		if len(found) == 1 && found[0].pid == cmd.Process.Pid {
			return cmd.Process.Pid
		}
	}
	t.Fatal("fake dispatcher could not be listed")
	return 0
}

// gone reports whether pid ended within five seconds; a zombie, which
// nothing may reap under a PID 1 that does not, has ended.
func gone(pid int) bool {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if !alive(pid) {
			return true
		}
	}
	return false
}

func alive(pid int) bool {
	if syscall.Kill(pid, 0) != nil {
		return false
	}
	out, err := exec.Command("ps", "-p", fmt.Sprint(pid), "-o", "stat=").Output()
	return err == nil && !strings.HasPrefix(strings.TrimSpace(string(out)), "Z")
}

func kill(pid int) { _ = syscall.Kill(pid, syscall.SIGKILL) }

// The runner stands for a test process that a timeout ends: it arms its
// state root, starts one dispatcher of it and one of the same binary for
// another run's state root, prints both pids and exits without t.Cleanup.
func TestRunnerThatExitsWithoutCleanup(t *testing.T) {
	dir := os.Getenv("TESTREAP_RUNNER")
	if dir == "" {
		t.Skip("run by TestArmEndsOnlyItsOwnDispatchersWhenTheTestProcessDies")
	}
	ours := filepath.Join(dir, "our state")
	Arm(t, ours)
	pid := startFake(t, ours)
	other := startFake(t, filepath.Join(dir, "other state"))
	fmt.Printf("%d %d\n", pid, other)
	os.Exit(3)
}

func TestArmEndsOnlyItsOwnDispatchersWhenTheTestProcessDies(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runner := exec.Command(exe, "-test.run", "^TestRunnerThatExitsWithoutCleanup$")
	runner.Env = append(os.Environ(), "TESTREAP_RUNNER="+t.TempDir())
	out, err := runner.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Start(); err != nil {
		t.Fatal(err)
	}
	var ours, other int
	line, _ := bufio.NewReader(out).ReadString('\n')
	_ = runner.Wait()
	if _, err := fmt.Sscan(line, &ours, &other); err != nil {
		t.Fatalf("runner printed %q", line)
	}
	defer kill(other)
	if !gone(ours) {
		kill(ours)
		t.Fatalf("dispatcher %d outlived the test process that armed it", ours)
	}
	time.Sleep(1500 * time.Millisecond) // the watchdog sweeps once a second
	if !alive(other) {
		t.Fatalf("the watchdog ended dispatcher %d of the same binary that it did not own", other)
	}
}

func TestCheckFailsAndEndsOnlyADispatcherItsRunLeft(t *testing.T) {
	dir := t.TempDir()
	var pid int
	t.Run("leaks", func(t *testing.T) {
		root := filepath.Join(dir, "state")
		Arm(t, root)
		pid = startFake(t, root)
	})
	other := startFake(t, filepath.Join(dir, "other state"))
	defer kill(other)
	// A finished test's watchdog stays armed: collecting its pipe must not
	// end the dispatcher before the run does.
	runtime.GC()
	time.Sleep(300 * time.Millisecond)
	if got := Check(0); got != 1 {
		t.Fatalf("Check returned %d with dispatcher %d left running", got, pid)
	}
	if !gone(pid) {
		t.Fatalf("Check left dispatcher %d running", pid)
	}
	if got := Check(0); got != 0 {
		t.Fatalf("Check returned %d with nothing of its own left", got)
	}
	if !alive(other) {
		t.Fatalf("Check ended dispatcher %d that its run did not start", other)
	}
}

func TestCheckFailsWhenItCannotListProcesses(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("Linux reads /proc, not ps")
	}
	root := filepath.Join(t.TempDir(), "state")
	Arm(t, root)
	pid := startFake(t, root)
	t.Cleanup(func() { // after PATH is back
		kill(pid)
		gone(pid)
	})
	broken := t.TempDir()
	if err := os.WriteFile(filepath.Join(broken, "ps"), []byte("#!/bin/sh\nexit 17\n"), 0700); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{"missing": t.TempDir(), "failing": broken} {
		t.Setenv("PATH", path)
		if got := Check(0); got != 1 {
			t.Fatalf("Check returned %d with ps %s and dispatcher %d running", got, name, pid)
		}
	}
}
