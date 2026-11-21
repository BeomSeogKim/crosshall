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
	gitDir := filepath.Join(root, ".git")
	info, err := os.Stat(gitDir)
	if err != nil {
		return root, false
	}
	if !info.IsDir() {
		if !info.Mode().IsRegular() || info.Size() > 8192 {
			return root, false
		}
		data, err := os.ReadFile(gitDir)
		if err != nil {
			return root, false
		}
		target, ok := strings.CutPrefix(strings.TrimRight(string(data), "\r\n"), "gitdir: ")
		if !ok || target == "" {
			return root, false
		}
		if gitDir = target; !filepath.IsAbs(gitDir) {
			gitDir = filepath.Join(root, gitDir)
		}
		// Only a linked worktree's Git directory names a common directory.
		if data, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil && len(data) <= 8192 {
			if common := strings.TrimRight(string(data), "\r\n"); common != "" {
				if !filepath.IsAbs(common) {
					common = filepath.Join(gitDir, common)
				}
				gitDir, linked = common, true
			}
		}
	}
	gitDir, err = filepath.EvalSymlinks(gitDir)
	if err != nil {
		return root, false
	}
	if _, err := os.Stat(filepath.Join(gitDir, "HEAD")); err != nil {
		return root, false
	}
	if filepath.Base(gitDir) == ".git" {
		return filepath.Dir(gitDir), linked
	}
	return gitDir, linked
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
