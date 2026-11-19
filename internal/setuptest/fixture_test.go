package setuptest

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestFixtureBoundsDefaultsOverridesAndMissingLeaves(t *testing.T) {
	f := New(t, "claude")
	if _, err := exec.LookPath("codex"); err == nil {
		t.Fatal("fixture found an unrequested vendor")
	}
	if out, err := exec.Command("claude", "--version").Output(); err != nil || string(out) != "2.1.327 (Claude Code)\n" {
		t.Fatalf("fixture claude --version = %q, %v", out, err)
	}
	t.Setenv("CODEX_HOME", filepath.Join(f.Root, "override", "yet to exist"))
	f.CheckTargets(t)
	link := filepath.Join(f.Home, ".claude")
	if err := os.Symlink(f.Outside, link); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{f.Outside, filepath.Join(link, "missing", "settings.json")} {
		if f.checkPath(path) == nil {
			t.Errorf("accepted fixture escape %s", path)
		}
	}
}
