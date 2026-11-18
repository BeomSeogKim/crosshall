package cli

import (
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

func TestOwnerRestoreUsesBindingAndKeepsNewKeyOutOfOutput(t *testing.T) {
	const secret = "fresh-session-key"
	ft := &fakeTransport{result: okResult(t, map[string]int{"binding_id": 73})}
	out, stderr, code := run(t, Env{Transport: ft}, "owner", "restore", "73", "--new-key", "codex:"+secret, "--key", "retry-1")
	if code != core.ExitOK || !strings.Contains(out, "73") || strings.Contains(out+stderr, secret) {
		t.Fatalf("restore exit %d, output %q %q", code, out, stderr)
	}
	if len(ft.calls) != 1 || !ft.calls[0].Owner || ft.calls[0].Command.Verb() != "owner restore" || ft.calls[0].OpKey != "retry-1" {
		t.Fatal("restore did not use the authenticated owner transport and original operation key")
	}
}

func TestOwnerRestoreInvalidArgumentsStayLocal(t *testing.T) {
	const secret = "sensitive-value"
	for _, args := range [][]string{
		{"0", "--new-key", "codex:" + secret},
		{"5", "--new-key", "other:" + secret},
		{"5", "--new-key", "codex:"},
		{"5", "--new-key", "route:" + secret},
		{"5", "--new-key", secret},
	} {
		ft := &fakeTransport{}
		out, stderr, code := run(t, Env{Transport: ft}, append([]string{"owner", "restore"}, args...)...)
		if code != core.ExitUsage || len(ft.calls) != 0 || strings.Contains(out+stderr, secret) {
			t.Fatalf("invalid restore published, leaked its key, or returned status %d", code)
		}
	}
}
