//go:build testvendorhome

package link

import (
	"errors"
	"os"
	"testing"
)

// A child that exits on its own leaves no pipe of this process open.
func TestAnEndedChildClosesItsPipes(t *testing.T) {
	t.Setenv(sshOverrideEnv, "/usr/bin/true")
	child, err := startSSH(t.Context(), "home.example", "crosshall", false)
	if err != nil {
		t.Fatal(err)
	}
	_ = child.wait()
	if _, err := child.stdout.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("stdout after wait: %v", err)
	}
	if _, err := child.stdin.Write([]byte{0}); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("stdin after wait: %v", err)
	}
}
