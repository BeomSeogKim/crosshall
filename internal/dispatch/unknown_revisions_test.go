package dispatch

import (
	"fmt"
	"os"
	"slices"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/spool"
)

func TestOwnerDoctorNamesTheRevisionsTheLiveDispatcherRefused(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	state, err := spool.OpenServerRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	if got := unknownRevisionWarnings(state, os.Getpid()); got != nil {
		t.Fatalf("no file: %v", got)
	}
	notes := &RevisionNotes{StateRoot: root}
	notes.Note("claude", "claude-9")
	notes.Note("claude", "claude-9")
	notes.Note("codex", "codex-7")
	want := []string{domain.UnknownRevisionMessage("claude-9"), domain.UnknownRevisionMessage("codex-7")}
	if got := unknownRevisionWarnings(state, os.Getpid()); !slices.Equal(got, want) {
		t.Fatalf("deduplicated warnings: %q", got)
	}
	for i := range 2 * maxUnknownRevisions {
		notes.Note("claude", fmt.Sprintf("claude-x%d", i))
	}
	if got := unknownRevisionWarnings(state, os.Getpid()); len(got) != maxUnknownRevisions || !slices.Equal(got[:2], want) {
		t.Fatalf("bounded warnings: %d %q", len(got), got)
	}
	// A restarted dispatcher has another PID: the file it did not write says nothing.
	if got := unknownRevisionWarnings(state, os.Getpid()+1); got != nil {
		t.Fatalf("after a restart: %v", got)
	}
}
