package manifests

import (
	"slices"
	"testing"
)

func TestRevisionsWithListsShippedAndPriorRevisions(t *testing.T) {
	got := RevisionsWith(func(c Capabilities) bool { return c.ProcessLaunch })
	want := []Revision{{"claude", "claude-1"}, {"claude", "claude-2"}, {"claude", "claude-3"}}
	if !slices.Equal(got, want) {
		t.Fatalf("process_launch revisions %v, want %v", got, want)
	}
	if got := RevisionsWith(func(Capabilities) bool { return false }); len(got) != 0 {
		t.Fatalf("no capability matched %v", got)
	}
}
