package store

import "testing"

// The revisions with exit authority are those whose manifests declare
// process_launch: today every Claude Code revision, and nothing else.
func TestProcessLaunchProfileNamesTheDeclaredRevisions(t *testing.T) {
	want := "((json_extract(p, '$.adapter') IN ('claude') AND json_extract(p, '$.manifest_rev') IN ('claude-1', 'claude-2', 'claude-3')))"
	if got := ProcessLaunchProfile("p"); got != want {
		t.Fatalf("predicate\n got %s\nwant %s", got, want)
	}
}
