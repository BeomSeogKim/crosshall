package dispatch

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/spool"
)

// macOS's daily /tmp cleaner removes a quiet member's empty spool
// directories and keeps the leaf, whose files are newer. The dispatcher
// recreates them once and says so once, instead of failing on every pass
// (#372).
func TestScanRestoresARemovedMemberSpoolOnce(t *testing.T) {
	s, root := adapterFilesFixture(t)
	if err := prepareNamedSpool(s.state, "owner"); err != nil {
		t.Fatal(err)
	}
	defer s.withdrawReady()
	leaf := adapterLeafFixture(t, s, root, true)
	if err := spool.PrepareServerSpool(leaf); err != nil {
		t.Fatal(err)
	}
	if err := s.scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"in", "work", "out"} {
		if err := os.Remove(filepath.Join(leaf.Name(), name)); err != nil {
			t.Fatal(err)
		}
	}

	var logged bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(previous)
	for range 3 {
		if err := s.scan(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"in", "work", "out"} {
		if fi, err := os.Lstat(filepath.Join(leaf.Name(), name)); err != nil || !fi.IsDir() || fi.Mode().Perm() != 0700 {
			t.Fatalf("%s after the scans: %v, %v", name, fi, err)
		}
	}
	lines := strings.Split(strings.TrimSpace(logged.String()), "\n")
	if len(lines) != 1 || !strings.Contains(lines[0], "recreated the removed spool of member directory "+leaf.Name()) {
		t.Fatalf("three scans logged %q, want one line naming the restored leaf", logged.String())
	}
}
