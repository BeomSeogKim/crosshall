package proc

import (
	"strconv"
	"strings"
	"testing"
)

func TestLocalProcFSRequiresTheCallerPIDNamespace(t *testing.T) {
	for _, tc := range []struct {
		name, status string
		local        bool
	}{
		{"local", "Pid:\t42\nNSpid:\t42\nNSpgid:\t1\n", true},
		{"ancestor mount", "NSpid:\t1300\t42\n", false},
		{"same number in ancestor", "NSpid:\t42\t42\n", false},
		{"wrong PID", "NSpid:\t43\n", false},
		{"missing", "Pid:\t42\n", false},
		{"malformed", "NSpid:\tnot-a-pid\n", false},
	} {
		if got := localProcFS(tc.status, 42); got != tc.local {
			t.Errorf("%s: local procfs = %t, want %t", tc.name, got, tc.local)
		}
	}
}

func TestPIDNamespaceIsStable(t *testing.T) {
	first := PIDNamespace()
	if first == "" {
		t.Skip("PID namespace metadata unavailable in this Linux environment")
	}
	boot, inode, ok := strings.Cut(first, ":")
	n, err := strconv.ParseUint(inode, 10, 64)
	if !ok || len(boot) != 36 || err != nil || n == 0 {
		t.Fatalf("invalid PID namespace identity %q", first)
	}
	if got := PIDNamespace(); got != first {
		t.Fatalf("PID namespace identity changed from %q to %q", first, got)
	}
}
