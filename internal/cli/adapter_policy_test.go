package cli

import (
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

func TestOwnerAdapterSetDispatchesTypedPolicy(t *testing.T) {
	ft := &fakeTransport{result: okResult(t, store.AdapterPolicy{Adapter: "codex", Enabled: true, Epoch: 3})}
	stdout, stderr, code := run(t, Env{Transport: ft}, "--key", "policy-key",
		"owner", "adapter", "set", "codex", "--enabled", "on", "--reminder=off")
	if code != core.ExitOK || stdout != "codex adapter enabled=on reminder=off epoch=3\n" || stderr != "" {
		t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	if len(ft.calls) != 1 {
		t.Fatalf("wrong calls: %+v", ft.calls)
	}
	call := ft.calls[0]
	cmd, ok := call.Command.(domain.AdapterSet)
	if !ok || cmd.Adapter != "codex" || cmd.Enabled == nil || !*cmd.Enabled ||
		cmd.Reminder == nil || *cmd.Reminder || !call.Owner || call.OpKey != "policy-key" {
		t.Fatalf("wrong call: %+v", call)
	}
}

func TestOwnerAdapterSetUsageRejectsInvalidOptions(t *testing.T) {
	ft := &fakeTransport{}
	for _, args := range [][]string{
		{"adapter"}, {"adapter", "other", "codex", "--enabled", "on"},
		{"adapter", "set", "codex"}, {"adapter", "set", "unknown", "--enabled", "on"},
		{"adapter", "set", "codex", "--enabled"}, {"adapter", "set", "codex", "--reminder=yes"},
		{"adapter", "set", "codex", "--enabled=off", "--enabled=on"},
		{"adapter", "set", "codex", "--unknown", "on"},
	} {
		_, _, code := run(t, Env{Transport: ft}, append([]string{"owner"}, args...)...)
		if code != core.ExitUsage {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if len(ft.calls) != 0 {
		t.Fatalf("usage errors reached adapter policy transport: %+v", ft.calls)
	}
}

func TestOwnerAdapterSetChecksOwnerGate(t *testing.T) {
	ft := &fakeTransport{}
	_, _, code := run(t, Env{Transport: ft, OwnerGate: func() string { return "owner shell required" }},
		"owner", "adapter", "set", "codex", "--enabled", "on")
	if code != core.ExitRefused || len(ft.calls) != 0 {
		t.Fatalf("exit %d with %d transport calls", code, len(ft.calls))
	}
}
