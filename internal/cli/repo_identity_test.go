package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRepoIdentityIsOneForEveryCheckoutOfARepository(t *testing.T) {
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
	// link lays out a worktree added to the repository whose Git directory is gitDir.
	link := func(gitDir, worktree string) {
		admin := filepath.Join(gitDir, "worktrees", filepath.Base(worktree))
		write(filepath.Join(admin, "HEAD"), "ref: refs/heads/side\n")
		write(filepath.Join(admin, "commondir"), "../..\n")
		write(filepath.Join(worktree, ".git"), "gitdir: "+admin+"\n")
	}
	main := filepath.Join(base, "main")
	write(filepath.Join(main, ".git", "HEAD"), "ref: refs/heads/main\n")
	linked := filepath.Join(base, "linked")
	link(filepath.Join(main, ".git"), linked)

	bare := filepath.Join(base, "store.git")
	write(filepath.Join(bare, "HEAD"), "ref: refs/heads/main\n")
	ofBare := filepath.Join(base, "of-bare")
	link(bare, ofBare)

	// git init --separate-git-dir: the main checkout's .git is a file too.
	metadata := filepath.Join(base, "metadata")
	write(filepath.Join(metadata, "HEAD"), "ref: refs/heads/main\n")
	apart := filepath.Join(base, "apart")
	write(filepath.Join(apart, ".git"), "gitdir: "+metadata+"\n")
	ofApart := filepath.Join(base, "of-apart")
	link(metadata, ofApart)

	// A submodule's Git directory names no common directory.
	module := filepath.Join(main, "module")
	moduleDir := filepath.Join(main, ".git", "modules", "module")
	write(filepath.Join(moduleDir, "HEAD"), "ref: refs/heads/main\n")
	write(filepath.Join(module, ".git"), "gitdir: ../.git/modules/module\n")

	for root, want := range map[string]struct {
		identity string
		linked   bool
	}{
		main: {main, false}, linked: {main, true},
		ofBare: {bare, true},
		apart:  {metadata, false}, ofApart: {metadata, true},
		module: {moduleDir, false},
		base:   {base, false},
	} {
		if identity, linked := RepoIdentity(root); identity != want.identity || linked != want.linked {
			t.Errorf("RepoIdentity(%s) = %s, %v; want %s, %v", root, identity, linked, want.identity, want.linked)
		}
	}
	// A default is kept under the repository for a main checkout and under
	// the worktree for a linked one.
	for path, want := range map[string]string{main: main, linked: linked, apart: metadata, ofApart: ofApart} {
		if got, err := RepoDefaultRoot(path); err != nil || got != want {
			t.Errorf("RepoDefaultRoot(%s) = %s, %v; want %s", path, got, err, want)
		}
	}
}
