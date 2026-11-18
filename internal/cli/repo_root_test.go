package cli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRepoRootCanonicalizesNestedAndSymlinkPaths(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "worktree")
	deep := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(deep, 0o700); err != nil {
		t.Fatal(err)
	}
	writeGitHEAD(t, filepath.Join(root, ".git"))
	link := filepath.Join(parent, "linked")
	if err := os.Symlink(deep, link); err != nil {
		t.Fatal(err)
	}
	canonical, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(deep)
	for _, input := range []string{"", ".", deep, link} {
		if got, err := RepoRoot(input); err != nil || got != canonical {
			t.Fatalf("RepoRoot(%q) = %q, %v, want %q", input, got, err, canonical)
		}
	}
}

func TestRepoRootAcceptsWorktreeGitFile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "metadata")
	writeGitHEAD(t, dir)
	file := filepath.Join(root, ".git")
	if err := os.WriteFile(file, []byte("gitdir: metadata\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := RepoRoot(root); err != nil || got != want {
		t.Fatalf("RepoRoot(%q) = %q, %v, want %q", root, got, err, want)
	}
	if _, err := RepoRoot(file); err == nil {
		t.Fatal("a file was accepted as a worktree directory")
	}
}

func TestRepoRootRejectsMissingAndInvalidWorktreeMarkers(t *testing.T) {
	root := t.TempDir()
	if _, err := RepoRoot(root); err == nil {
		t.Fatal("a directory outside any worktree was accepted")
	}
	writeGitHEAD(t, filepath.Join(root, ".git"))
	nested := filepath.Join(root, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(nested, ".git"), []byte("invalid\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := RepoRoot(nested); err == nil {
		t.Fatal("an invalid nested worktree marker was ignored")
	}
}

func writeGitHEAD(t *testing.T, directory string) {
	t.Helper()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "HEAD"), []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}
