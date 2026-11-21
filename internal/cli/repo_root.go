package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RepoRoot returns the canonical root of the nearest enclosing Git worktree.
func RepoRoot(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	root, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(root)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("repository path %q must be a directory", path)
	}
	for dir := root; ; dir = filepath.Dir(dir) {
		found, err := worktreeMarker(filepath.Join(dir, ".git"))
		if err != nil {
			return "", err
		}
		if found {
			return dir, nil
		}
		if filepath.Dir(dir) == dir {
			break
		}
	}
	return "", fmt.Errorf("no Git worktree encloses %q", root)
}

// RepoIdentity names the whole repository of the worktree at root, the same
// for its main checkout and every linked worktree: the directory that holds
// the common Git directory when that one is called .git, else the common Git
// directory itself (a bare repository, or one made with --separate-git-dir).
// linked reports a worktree added to another checkout's repository.
func RepoIdentity(root string) (identity string, linked bool) {
	common, linked, ok := commonGitDir(root)
	if !ok {
		return root, false
	}
	if filepath.Base(common) == ".git" {
		return filepath.Dir(common), linked
	}
	return common, linked
}

// commonGitDir finds the canonical common Git directory of the worktree at root.
func commonGitDir(root string) (common string, linked, ok bool) {
	gitDir := filepath.Join(root, ".git")
	info, err := os.Stat(gitDir)
	if err != nil {
		return "", false, false
	}
	if !info.IsDir() {
		if !info.Mode().IsRegular() || info.Size() > 8192 {
			return "", false, false
		}
		data, err := os.ReadFile(gitDir)
		if err != nil {
			return "", false, false
		}
		target, found := strings.CutPrefix(strings.TrimRight(string(data), "\r\n"), "gitdir: ")
		if !found || target == "" {
			return "", false, false
		}
		if gitDir = target; !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(root, gitDir)
		}
		// Only a linked worktree's Git directory names a common directory.
		if data, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil && len(data) <= 8192 {
			if dir := strings.TrimRight(string(data), "\r\n"); dir != "" {
				if !filepath.IsAbs(dir) {
					dir = filepath.Join(gitDir, dir)
				}
				gitDir, linked = dir, true
			}
		}
	}
	gitDir, err = filepath.EvalSymlinks(gitDir)
	if err != nil {
		return "", false, false
	}
	if _, err := os.Stat(filepath.Join(gitDir, "HEAD")); err != nil {
		return "", false, false
	}
	return gitDir, linked, true
}

// IsWorkTree reports whether path lies in the work tree of a real
// repository: its common Git directory holds objects and refs, as Git itself
// requires, and path is not inside the checkout's own .git directory.
func IsWorkTree(path string) bool {
	root, err := RepoRoot(path)
	if err != nil {
		return false
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return false
	}
	if absolute, err = filepath.EvalSymlinks(absolute); err != nil {
		return false
	}
	admin := filepath.Join(root, ".git")
	if absolute == admin || strings.HasPrefix(absolute, admin+string(filepath.Separator)) {
		return false
	}
	common, _, ok := commonGitDir(root)
	if !ok {
		return false
	}
	for _, name := range []string{"objects", "refs"} {
		if info, err := os.Stat(filepath.Join(common, name)); err != nil || !info.IsDir() {
			return false
		}
	}
	return true
}

// RepoDefaultRoot is the root a repository default is kept under for the
// worktree enclosing path: the repository for its main checkout, so that
// linked worktrees follow it, and the worktree itself for a linked one.
func RepoDefaultRoot(path string) (string, error) {
	root, err := RepoRoot(path)
	if err != nil {
		return "", err
	}
	if identity, linked := RepoIdentity(root); !linked {
		return identity, nil
	}
	return root, nil
}

func worktreeMarker(marker string) (bool, error) {
	info, err := os.Stat(marker)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	dir := marker
	if !info.IsDir() {
		if !info.Mode().IsRegular() || info.Size() > 8192 {
			return false, fmt.Errorf("invalid Git marker %q", marker)
		}
		data, err := os.ReadFile(marker)
		if err != nil {
			return false, err
		}
		target, ok := strings.CutPrefix(strings.TrimRight(string(data), "\r\n"), "gitdir: ")
		if !ok || target == "" {
			return false, fmt.Errorf("invalid Git marker %q", marker)
		}
		dir = target
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(filepath.Dir(marker), dir)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "HEAD")); err != nil {
		return false, fmt.Errorf("invalid Git directory for %q: %w", marker, err)
	}
	return true, nil
}
