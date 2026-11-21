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
	// Git's own directories are in no work tree, wherever they live.
	inside := filepath.Join(apart, "inside")
	write(filepath.Join(inside, "HEAD"), "ref: refs/heads/main\n")
	within := filepath.Join(base, "within")
	write(filepath.Join(within, ".git"), "gitdir: inside\n")
	write(filepath.Join(within, "inside", "HEAD"), "ref: refs/heads/main\n")
	for _, dir := range []string{
		filepath.Join(main, ".git", "objects"), filepath.Join(main, "src"), filepath.Join(linked, "src"),
		filepath.Join(within, "inside", "objects"), filepath.Join(within, "src"),
	} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for path, want := range map[string]string{
		main: main, filepath.Join(main, "src"): main, linked: linked, filepath.Join(linked, "src"): linked,
		apart: apart, within: within, filepath.Join(within, "src"): within,
		filepath.Join(main, ".git"):                        "",
		filepath.Join(main, ".git", "objects"):             "",
		filepath.Join(main, ".git", "worktrees", "linked"): "",
		filepath.Join(within, "inside"):                    "",
		filepath.Join(within, "inside", "objects"):         "",
		metadata: "", bare: "", base: "",
	} {
		if got := WorkTree(path); got != want {
			t.Errorf("WorkTree(%s) = %q, want %q", path, got, want)
		}
	}
	// Git accepts a repository only with its objects and refs.
	if HasObjectStore(main) || HasObjectStore(base) {
		t.Error("a Git directory with only a HEAD passed as a repository")
	}
	for _, name := range []string{"objects", "refs"} {
		if err := os.MkdirAll(filepath.Join(main, ".git", name), 0700); err != nil {
			t.Fatal(err)
		}
	}
	if !HasObjectStore(main) || !HasObjectStore(linked) || HasObjectStore(apart) {
		t.Error("HasObjectStore does not follow the common Git directory")
	}
	// A default is kept under the repository for a main checkout and under
	// the worktree for a linked one.
	for path, want := range map[string]string{main: main, linked: linked, apart: metadata, ofApart: ofApart} {
		if got, err := RepoDefaultRoot(path); err != nil || got != want {
			t.Errorf("RepoDefaultRoot(%s) = %s, %v; want %s", path, got, err, want)
		}
	}
}
