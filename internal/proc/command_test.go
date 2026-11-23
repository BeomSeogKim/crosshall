//go:build darwin || linux

package proc

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// A process's command is the executable the kernel ran and its arguments,
// as the process itself sees them.
func TestProcessCommandReadsExecutableAndArguments(t *testing.T) {
	exe, args, err := processCommand(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := filepath.EvalSymlinks(exe)
	want, _ = filepath.EvalSymlinks(want)
	if got != want || !slices.Equal(args, os.Args) {
		t.Fatalf("command = %q %q, want %q %q", exe, args, want, os.Args)
	}
	if _, _, err := processCommand(1 << 30); err == nil {
		t.Fatal("a missing process has a command")
	}
}

// The host of a test is the process that ran it, which is not a shell.
func TestHostCommandSkipsNoProcessThatIsNotAShell(t *testing.T) {
	pid, exe, args, ok := HostCommand()
	if !ok || pid <= 1 || !filepath.IsAbs(exe) || len(args) == 0 {
		t.Fatalf("host command = %d %q %q, %v", pid, exe, args, ok)
	}
	parent, parentArgs, err := processCommand(os.Getppid())
	if err != nil {
		t.Fatal(err)
	}
	name, _, _ := processParent(os.Getppid())
	if !shells[filepath.Base(name)] && (pid != os.Getppid() || exe != parent || !slices.Equal(args, parentArgs)) {
		t.Fatalf("host command = %q, want the parent %q", exe, parent)
	}
}
