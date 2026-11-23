//go:build darwin || linux

package hook

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/proc"
)

// The test's parent stands in for the agent: it is an ancestor of this
// process. A process that is not an ancestor, or a crosshall run host whose
// start differs, names no host.
func TestMemberHostNamesAnAncestorOnly(t *testing.T) {
	parent := os.Getppid()
	start, err := proc.StartOf(parent)
	if err != nil {
		t.Fatal(err)
	}
	sleeper := exec.Command("sleep", "30")
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() }()
	other, err := proc.StartOf(sleeper.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	env := func(name, value string) func(string) (string, bool) {
		return func(k string) (string, bool) {
			if k == name {
				return value, true
			}
			return "", false
		}
	}
	route := ident.Identity{Vendor: "route", Key: "k"}
	claude := ident.Identity{Vendor: "claude", Key: "k"}
	for _, c := range []struct {
		name   string
		id     ident.Identity
		lookup func(string) (string, bool)
		want   int
	}{
		{"run host", route, env(ident.HostVariable, fmt.Sprintf("%d:%d", parent, start.Identity)), parent},
		{"run host with another start", route, env(ident.HostVariable, fmt.Sprintf("%d:%d", parent, start.Identity+1)), 0},
		{"run host not an ancestor", route, env(ident.HostVariable, fmt.Sprintf("%d:%d", sleeper.Process.Pid, other.Identity)), 0},
		{"run without the variable", route, env("", ""), 0},
		{"claude", claude, env("CLAUDE_PID", strconv.Itoa(parent)), parent},
		{"claude not an ancestor", claude, env("CLAUDE_PID", strconv.Itoa(sleeper.Process.Pid)), 0},
		{"codex names none yet", ident.Identity{Vendor: "codex", Key: "k"}, env("CLAUDE_PID", strconv.Itoa(parent)), 0},
	} {
		h := MemberHost(c.id, c.lookup)
		switch {
		case c.want == 0 && h != nil:
			t.Errorf("%s: host %+v, want none", c.name, h)
		case c.want != 0 && (h == nil || h.PID != int64(c.want) || h.Start != start.Identity || h.NS != proc.PIDNamespace()):
			t.Errorf("%s: host %+v, want pid %d start %d", c.name, h, c.want, start.Identity)
		}
	}
}
