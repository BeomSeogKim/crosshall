//go:build darwin || linux

package proc

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The child runs under a shell under this test: the test process is its
// nearest non-shell ancestor; the test's own parent is not.
func TestHostedBySkipsShellsOnly(t *testing.T) {
	if want := os.Getenv("PROC_HOSTED_WANT"); want != "" {
		pid, _ := strconv.Atoi(want)
		os.Stdout.WriteString(strconv.FormatBool(HostedBy(pid)))
		os.Exit(0)
	}
	for _, c := range []struct {
		pid  int
		want string
	}{{os.Getpid(), "true"}, {os.Getppid(), "false"}} {
		cmd := exec.Command("sh", "-c", `"$0" -test.run=TestHostedBySkipsShellsOnly; true`, os.Args[0])
		cmd.Env = append(os.Environ(), "PROC_HOSTED_WANT="+strconv.Itoa(c.pid))
		out, err := cmd.Output()
		if err != nil || strings.TrimSpace(string(out)) != c.want {
			t.Fatalf("HostedBy(%d) under sh = %q (%v), want %s", c.pid, out, err, c.want)
		}
	}
}

// TestHostFindsTheAgentAboveAWrapper runs the check under a shell under a
// wrapper that is not a shell (this test binary again): the agent above
// both is found by its PID or by its name; a process that is not an
// ancestor, and a name no ancestor has, are not.
func TestHostFindsTheAgentAboveAWrapper(t *testing.T) {
	switch os.Getenv("PROC_HOST_MODE") {
	case "wrap":
		cmd := exec.Command("sh", "-c", `"$0" -test.run=TestHostFindsTheAgentAboveAWrapper; true`, os.Args[0])
		cmd.Env = append(os.Environ(), "PROC_HOST_MODE=check")
		cmd.Stdout = os.Stdout
		_ = cmd.Run()
		os.Exit(0)
	case "check":
		pid, _ := strconv.Atoi(os.Getenv("PROC_HOST_PID"))
		var names []string
		if n := os.Getenv("PROC_HOST_NAME"); n != "" {
			names = []string{n}
		}
		found, start, ok := Host(pid, names)
		os.Stdout.WriteString(strconv.FormatBool(ok) + " " + strconv.Itoa(found) + " " + strconv.FormatInt(start.Identity, 10))
		os.Exit(0)
	}
	self, err := StartOf(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	sleeper := exec.Command("sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() }()
	run := func(pid int, name string) []string {
		t.Helper()
		cmd := exec.Command(os.Args[0], "-test.run=TestHostFindsTheAgentAboveAWrapper")
		cmd.Env = append(os.Environ(), "PROC_HOST_MODE=wrap", "PROC_HOST_PID="+strconv.Itoa(pid), "PROC_HOST_NAME="+name)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.Fields(string(out))
	}
	if got := run(os.Getpid(), ""); len(got) != 3 || got[0] != "true" || got[1] != strconv.Itoa(os.Getpid()) ||
		got[2] != strconv.FormatInt(self.Identity, 10) {
		t.Errorf("by PID: %v, want true %d %d", got, os.Getpid(), self.Identity)
	}
	if got := run(sleeper.Process.Pid, ""); len(got) == 0 || got[0] != "false" {
		t.Errorf("a process that is not an ancestor: %v", got)
	}
	// The nearest ancestor with this binary's name is the wrapper, above
	// the shell; the test process further up has the same name.
	if got := run(0, filepath.Base(os.Args[0])); len(got) == 0 || got[0] != "true" || got[1] == strconv.Itoa(os.Getpid()) {
		t.Errorf("by name: %v, want the wrapper", got)
	}
	if got := run(0, "no-such-agent"); len(got) == 0 || got[0] != "false" {
		t.Errorf("a name no ancestor has: %v", got)
	}
}
