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

// RepoCommonRoot returns the root that names the whole repository of the
// worktree at root: the main worktree's root for a linked worktree, or the
// common Git directory when that one is bare. Any other root names itself.
func RepoCommonRoot(root string) string {
	marker := filepath.Join(root, ".git")
	info, err := os.Stat(marker)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 8192 {
		return root
	}
	data, err := os.ReadFile(marker)
	if err != nil {
		return root
	}
	gitDir, ok := strings.CutPrefix(strings.TrimRight(string(data), "\r\n"), "gitdir: ")
	if !ok || gitDir == "" {
		return root
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(root, gitDir)
	}
	// Only a linked worktree's Git directory names a common directory.
	data, err = os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil || len(data) > 8192 {
		return root
	}
	common := strings.TrimRight(string(data), "\r\n")
	if common == "" {
		return root
	}
	if !filepath.IsAbs(common) {
		common = filepath.Join(gitDir, common)
	}
	common, err = filepath.EvalSymlinks(common)
	if err != nil {
		return root
	}
	if _, err := os.Stat(filepath.Join(common, "HEAD")); err != nil {
		return root
	}
	if filepath.Base(common) == ".git" {
		return filepath.Dir(common)
	}
	return common
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
