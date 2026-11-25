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

// startFake runs this binary as a dispatcher with env added to this
// process's environment, and returns once it runs.
func startFake(t *testing.T, env ...string) int {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "__dispatch")
	cmd.Env = append(os.Environ(), env...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go cmd.Wait()
	// The fake has exec'd once ps shows its argument.
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		out, _ := exec.Command("ps", "-ww", "-p", fmt.Sprint(cmd.Process.Pid), "-o", "args=").Output()
		if strings.HasSuffix(strings.TrimSpace(string(out)), " __dispatch") {
			return cmd.Process.Pid
		}
	}
	t.Fatal("fake dispatcher did not start")
	return 0
}

// strangers are dispatchers of another run, which must survive: one with
// its own token, and one whose values only contain this run's entry, which a
// matcher that does not keep entry boundaries would take for its own.
func strangers(t *testing.T) []int {
	return []int{
		startFake(t, Key+"=0123456789abcdef0123456789abcdef"),
		startFake(t, Key+"="+token+"x", "DESC=a\n"+Env()+"\nb", "NOTE=x "+Env()+"y"),
	}
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

// The runner stands for a test process that a timeout ends: it arms, starts
// one dispatcher of its own and the strangers, prints their pids and exits
// without t.Cleanup.
func TestRunnerThatExitsWithoutCleanup(t *testing.T) {
	if os.Getenv("TESTREAP_RUNNER") == "" {
		t.Skip("run by TestArmEndsOnlyItsOwnDispatchersWhenTheTestProcessDies")
	}
	Arm(t)
	pids := append([]int{startFake(t)}, strangers(t)...)
	fmt.Println(strings.Trim(fmt.Sprint(pids), "[]"))
	os.Exit(3)
}

func TestArmEndsOnlyItsOwnDispatchersWhenTheTestProcessDies(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	runner := exec.Command(exe, "-test.run", "^TestRunnerThatExitsWithoutCleanup$")
	runner.Env = append(os.Environ(), "TESTREAP_RUNNER=1")
	out, err := runner.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Start(); err != nil {
		t.Fatal(err)
	}
	var ours, other, lookalike int
	line, _ := bufio.NewReader(out).ReadString('\n')
	_ = runner.Wait()
	if _, err := fmt.Sscan(line, &ours, &other, &lookalike); err != nil {
		t.Fatalf("runner printed %q", line)
	}
	defer kill(other)
	defer kill(lookalike)
	if !gone(ours) {
		kill(ours)
		t.Fatalf("dispatcher %d outlived the test process that armed it", ours)
	}
	time.Sleep(1500 * time.Millisecond) // the watchdog sweeps once a second
	for _, pid := range []int{other, lookalike} {
		if !alive(pid) {
			t.Errorf("the watchdog ended dispatcher %d of another run", pid)
		}
	}
}

func TestCheckFailsAndEndsOnlyADispatcherItsRunLeft(t *testing.T) {
	var pid int
	t.Run("leaks", func(t *testing.T) {
		Arm(t)
		pid = startFake(t)
	})
	others := strangers(t)
	for _, other := range others {
		defer kill(other)
	}
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
	for _, other := range others {
		if !alive(other) {
			t.Errorf("Check ended dispatcher %d of another run", other)
		}
	}
}

func TestCheckFailsWhenItCannotListProcesses(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("Linux reads /proc, not ps")
	}
	Arm(t)
	pid := startFake(t)
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
