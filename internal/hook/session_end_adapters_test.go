package hook

import (
	"slices"
	"testing"
)

// Claude Code and Codex install a session-end hook; Antigravity's only
// installed event is its start.
func TestSessionEndAdapters(t *testing.T) {
	if got := SessionEndAdapters(); !slices.Equal(got, []string{"claude", "codex"}) {
		t.Fatalf("session-end adapters %v, want [claude codex]", got)
	}
}
