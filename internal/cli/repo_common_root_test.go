package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRepoCommonRootNamesTheWholeRepository(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	write := func(path, text string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	main := filepath.Join(base, "main")
	write(filepath.Join(main, ".git", "HEAD"), "ref: refs/heads/main\n")
	linked := filepath.Join(base, "linked")
	admin := filepath.Join(main, ".git", "worktrees", "linked")
	write(filepath.Join(admin, "HEAD"), "ref: refs/heads/side\n")
	write(filepath.Join(admin, "commondir"), "../..\n")
	write(filepath.Join(linked, ".git"), "gitdir: "+admin+"\n")

	bare := filepath.Join(base, "store.git")
	write(filepath.Join(bare, "HEAD"), "ref: refs/heads/main\n")
	ofBare := filepath.Join(base, "of-bare")
	bareAdmin := filepath.Join(bare, "worktrees", "of-bare")
	write(filepath.Join(bareAdmin, "HEAD"), "ref: refs/heads/main\n")
	write(filepath.Join(bareAdmin, "commondir"), "../..\n")
	write(filepath.Join(ofBare, ".git"), "gitdir: "+bareAdmin+"\n")

	// A submodule's Git directory names no common directory.
	module := filepath.Join(main, "module")
	write(filepath.Join(main, ".git", "modules", "module", "HEAD"), "ref: refs/heads/main\n")
	write(filepath.Join(module, ".git"), "gitdir: ../.git/modules/module\n")

	for root, want := range map[string]string{main: main, linked: main, ofBare: bare, module: module, base: base} {
		if got := RepoCommonRoot(root); got != want {
			t.Errorf("RepoCommonRoot(%s) = %s, want %s", root, got, want)
		}
	}
}
