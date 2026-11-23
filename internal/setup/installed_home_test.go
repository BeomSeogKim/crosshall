package setup

import (
	"os"
	"path/filepath"
	"testing"
)

// The dispatcher proves a Codex server against the home setup installed
// the hooks into, as the install journal records it (#236).
func TestInstalledConfigDirIsTheJournaledHome(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err == nil {
		err = os.Chmod(root, 0o700)
	}
	if err != nil {
		t.Fatal(err)
	}
	if got := InstalledConfigDir(root, "codex"); got != "" {
		t.Fatalf("no journal: %q", got)
	}
	state, err := openSetupState(root, true)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	j := emptyJournal()
	j.Files["codex"] = ownedFile{Path: "/home/u/.codex/hooks.json"}
	if err := writeJournal(state, j); err != nil {
		t.Fatal(err)
	}
	if got := InstalledConfigDir(root, "codex"); got != "/home/u/.codex" {
		t.Fatalf("codex home %q", got)
	}
	if got := InstalledConfigDir(root, "claude"); got != "" {
		t.Fatalf("claude was never installed, but reads %q", got)
	}
}
