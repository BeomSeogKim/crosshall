package testreap

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "__dispatch" {
		time.Sleep(time.Minute) // a fixture dispatcher that never stops by itself
		os.Exit(0)
	}
	os.Exit(Check(m.Run()))
}

// startFake runs exe as a dispatcher that does not stop by itself.
func startFake(t *testing.T, exe string) int {
	t.Helper()
	cmd := exec.Command(exe, "__dispatch")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go cmd.Wait()
	return cmd.Process.Pid
}

func gone(pid int) bool {
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if syscall.Kill(pid, 0) != nil {
			return true
		}
	}
	return false
}

func self(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

// The runner stands for a test process that a timeout ends: it arms a copy
// of this binary, starts it as a dispatcher, prints its pid and exits
// without running t.Cleanup.
func TestRunnerThatExitsWithoutCleanup(t *testing.T) {
	dir := os.Getenv("TESTREAP_RUNNER")
	if dir == "" {
		t.Skip("run by TestArmEndsTheDispatcherOfATestProcessThatDies")
	}
	source, err := os.Open(self(t))
	if err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(dir, "fixture bin", "crosshall")
	if err := os.Mkdir(filepath.Dir(exe), 0700); err != nil {
		t.Fatal(err)
	}
	dest, err := os.OpenFile(exe, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0700)
	if err == nil {
		_, err = io.Copy(dest, source)
		if closeErr := dest.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	Arm(t, exe)
	os.Stdout.WriteString(strconv.Itoa(startFake(t, exe)) + "\n")
	os.Exit(3)
}

func TestArmEndsTheDispatcherOfATestProcessThatDies(t *testing.T) {
	runner := exec.Command(self(t), "-test.run", "^TestRunnerThatExitsWithoutCleanup$")
	runner.Env = append(os.Environ(), "TESTREAP_RUNNER="+t.TempDir())
	out, err := runner.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := runner.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(out).ReadString('\n')
	_ = runner.Wait()
	pid, err := strconv.Atoi(line[:max(len(line)-1, 0)])
	if err != nil {
		t.Fatalf("runner printed %q", line)
	}
	if !gone(pid) {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("dispatcher %d outlived the test process that armed it", pid)
	}
}

func TestCheckFailsAndEndsADispatcherACleanupLeft(t *testing.T) {
	var pid int
	t.Run("leaks", func(t *testing.T) {
		Arm(t, self(t))
		pid = startFake(t, self(t))
	})
	if got := Check(0); got != 1 {
		t.Fatalf("Check returned %d with dispatcher %d left running", got, pid)
	}
	if !gone(pid) {
		t.Fatalf("Check left dispatcher %d running", pid)
	}
	if got := Check(0); got != 0 {
		t.Fatalf("Check returned %d with nothing left", got)
	}
}
