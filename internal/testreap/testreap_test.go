package testreap

import (
	"bufio"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "__dispatch" {
		// A fixture dispatcher that never stops by itself and names itself
		// in its ready marker.
		marker := fmt.Sprintf(`{"instance":"fake","v":1,"pid":%d}`, os.Getpid())
		if err := os.WriteFile(os.Getenv("TESTREAP_READY"), []byte(marker), 0600); err != nil {
			os.Exit(1)
		}
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(Check(m.Run()))
}

// startFake runs this binary as a dispatcher whose ready marker is ready,
// and returns once the marker names it.
func startFake(t *testing.T, ready string) int {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "__dispatch")
	cmd.Env = append(os.Environ(), "TESTREAP_READY="+ready)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go cmd.Wait()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if _, ok := named(ready); ok {
			return cmd.Process.Pid
		}
	}
	t.Fatal("fake dispatcher did not write its marker")
	return 0
}

// gone reports whether pid ended within five seconds; a zombie has ended.
func gone(pid int) bool {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "stat=").Output()
		if err != nil || strings.HasPrefix(strings.TrimSpace(string(out)), "Z") {
			return true
		}
	}
	return false
}

func kill(pid int) { _ = syscall.Kill(pid, syscall.SIGKILL) }

// The runner stands for a test process that a timeout ends: it arms one
// fake dispatcher's marker, starts another of the same binary that another
// run owns, prints both pids and exits without running t.Cleanup.
func TestRunnerThatExitsWithoutCleanup(t *testing.T) {
	dir := os.Getenv("TESTREAP_RUNNER")
	if dir == "" {
		t.Skip("run by TestArmEndsOnlyItsOwnDispatchersWhenTheTestProcessDies")
	}
	ours := filepath.Join(dir, "ours-ready")
	Arm(t, ours)
	pid := startFake(t, ours)
	other := startFake(t, filepath.Join(dir, "other-ready"))
	time.Sleep(300 * time.Millisecond) // let Arm learn its dispatcher
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
	if syscall.Kill(other, 0) != nil {
		t.Fatalf("the watchdog ended dispatcher %d of the same binary that it did not own", other)
	}
}

func TestCheckFailsAndEndsOnlyADispatcherItsRunLeft(t *testing.T) {
	dir := t.TempDir()
	var pid int
	t.Run("leaks", func(t *testing.T) {
		ready := filepath.Join(dir, "ready")
		Arm(t, ready)
		pid = startFake(t, ready)
		time.Sleep(300 * time.Millisecond)
	})
	other := startFake(t, filepath.Join(dir, "other-ready"))
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
	if syscall.Kill(other, 0) != nil {
		t.Fatalf("Check ended dispatcher %d that its run did not start", other)
	}
}

func TestCheckFailsWhenItCannotListProcesses(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if got := Check(0); got != 1 {
		t.Fatalf("Check returned %d without ps", got)
	}
}
