package cli

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// repoRoot returns the canonical root of the nearest enclosing Git worktree.
func repoRoot(path string) (string, error) {
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
