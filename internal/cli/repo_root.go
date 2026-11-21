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
	_, common, linked, ok := gitDirs(root)
	if !ok {
		return root, false
	}
	if filepath.Base(common) == ".git" {
		return filepath.Dir(common), linked
	}
	return common, linked
}

// gitDirs finds the canonical Git directories of the worktree at root: its
// own, and the repository's common one (the same unless root is linked).
func gitDirs(root string) (own, common string, linked, ok bool) {
	own = filepath.Join(root, ".git")
	info, err := os.Stat(own)
	if err != nil {
		return "", "", false, false
	}
	common = own
	if !info.IsDir() {
		if !info.Mode().IsRegular() || info.Size() > 8192 {
			return "", "", false, false
		}
		data, err := os.ReadFile(own)
		if err != nil {
			return "", "", false, false
		}
		target, found := strings.CutPrefix(strings.TrimRight(string(data), "\r\n"), "gitdir: ")
		if !found || target == "" {
			return "", "", false, false
		}
		if own = target; !filepath.IsAbs(own) {
			own = filepath.Join(root, own)
		}
		common = own
		// Only a linked worktree's Git directory names a common directory.
		// One that cannot be read leaves the repository unknown: taking
		// the worktree's own Git directory for it would name another.
		data, err = os.ReadFile(filepath.Join(own, "commondir"))
		if err != nil && !errors.Is(err, os.ErrNotExist) || len(data) > 8192 {
			return "", "", false, false
		}
		if dir := strings.TrimRight(string(data), "\r\n"); dir != "" {
			if !filepath.IsAbs(dir) {
				dir = filepath.Join(own, dir)
			}
			common, linked = dir, true
		}
	}
	if own, err = filepath.EvalSymlinks(own); err != nil {
		return "", "", false, false
	}
	if common, err = filepath.EvalSymlinks(common); err != nil {
		return "", "", false, false
	}
	if _, err := os.Stat(filepath.Join(common, "HEAD")); err != nil {
		return "", "", false, false
	}
	return own, common, linked, true
}

// WorkTree returns the root of the work tree that path lies in, as Git
// itself decides it (rev-parse --is-inside-work-tree), without running Git.
// It walks up from path like Git's discovery: a directory with a .git entry
// is a work tree's root, and a directory that is itself a Git directory ends
// the search with no work tree, whoever it belongs to: a bare repository, or
// another checkout's separate Git directory, nested in a checkout. It also
// returns "" inside the Git directories of the root it found: the
// checkout's own and the repository's common one. Nobody works in those, so
// a session there is in no repository.
func WorkTree(path string) string {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return ""
	}
	if absolute, err = filepath.EvalSymlinks(absolute); err != nil {
		return ""
	}
	root := absolute
	for {
		found, err := worktreeMarker(filepath.Join(root, ".git"))
		if err != nil {
			return ""
		}
		if found {
			break
		}
		if gitDirectory(root) || filepath.Dir(root) == root {
			return ""
		}
		root = filepath.Dir(root)
	}
	dirs := []string{filepath.Join(root, ".git")}
	if own, common, _, ok := gitDirs(root); ok {
		dirs = append(dirs, own, common)
	}
	for _, dir := range dirs {
		if absolute == dir || strings.HasPrefix(absolute, dir+string(filepath.Separator)) {
			return ""
		}
	}
	return root
}

// gitDirectory reports whether dir is itself a Git directory by Git's own
// test: a HEAD, with objects and refs beside it or in the common directory
// it names.
func gitDirectory(dir string) bool {
	head := filepath.Join(dir, "HEAD")
	info, err := os.Lstat(head)
	if err != nil {
		return false
	}
	common := dir
	if data, err := os.ReadFile(filepath.Join(dir, "commondir")); err == nil && len(data) <= 8192 {
		if named := strings.TrimRight(string(data), "\r\n"); named != "" {
			if common = named; !filepath.IsAbs(common) {
				common = filepath.Join(dir, common)
			}
		}
	}
	for _, name := range []string{"objects", "refs"} {
		if info, err := os.Stat(filepath.Join(common, name)); err != nil || !info.IsDir() {
			return false
		}
	}
	// A readable HEAD must look like one; an unreadable one counts.
	if info.Mode().IsRegular() && info.Size() <= 8192 {
		if data, err := os.ReadFile(head); err == nil {
			text := strings.TrimSpace(string(data))
			return strings.HasPrefix(text, "ref:") || len(text) >= 40 && strings.Trim(text, "0123456789abcdef") == ""
		}
	}
	return true
}

// HasObjectStore reports whether the worktree at root belongs to a
// repository Git itself accepts: its common Git directory holds objects and
// refs besides a HEAD.
func HasObjectStore(root string) bool {
	_, common, _, ok := gitDirs(root)
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
