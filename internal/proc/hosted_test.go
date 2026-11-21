//go:build darwin || linux

package proc

import (
	"os"
	"os/exec"
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
