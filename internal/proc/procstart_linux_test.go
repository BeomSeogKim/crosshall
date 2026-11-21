package proc

import (
	"errors"
	"os"
	"testing"
)

func TestMissingProcStatRequiresKernelProof(t *testing.T) {
	child := startProbeChild(t)
	pid := child.Process.Pid
	if err := procStatError(pid, os.ErrNotExist); !errors.Is(err, os.ErrNotExist) || errors.Is(err, errProcessGone) {
		t.Fatalf("missing stat for a live PID is %v, want an unknown process state", err)
	}
	if err := child.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = child.Wait()
	if err := procStatError(pid, os.ErrNotExist); !errors.Is(err, errProcessGone) {
		t.Fatalf("missing stat for an absent PID is %v, want process gone", err)
	}
	if err := procStatError(pid, os.ErrPermission); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("permission error for an absent PID is %v, want permission error", err)
	}
}
