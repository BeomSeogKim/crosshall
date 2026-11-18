// Package dispatch defines dispatcher startup checks and command handoff.
package dispatch

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

var errDefaultState = errors.New("development build requires CROSSHALL_STATE_DIR outside the default state root")

// DefaultStateRoot is the tagged build's normal store for this user.
func DefaultStateRoot() (string, error) {
	if xdg := os.Getenv("XDG_STATE_HOME"); filepath.IsAbs(xdg) {
		return filepath.Join(xdg, "crosshall"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot find the default state directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "crosshall"), nil
}

// CheckStateRoot keeps development and test builds out of the default store.
// It compares physical directories without rewriting the path the spool opens.
func CheckStateRoot(version, stateRoot, defaultStateRoot string) error {
	switch version {
	case "", "dev", "test":
	default:
		return nil
	}
	if defaultStateRoot == "" {
		var err error
		defaultStateRoot, err = DefaultStateRoot()
		if err != nil {
			return err
		}
	}
	if !filepath.IsAbs(stateRoot) || !filepath.IsAbs(defaultStateRoot) || filepath.Clean(stateRoot) == filepath.Clean(defaultStateRoot) {
		return errDefaultState
	}
	selected, tail, err := existingAncestor(stateRoot)
	if err != nil {
		return fmt.Errorf("check selected state root: %w", err)
	}
	standard, defaultTail, err := existingAncestor(defaultStateRoot)
	if err != nil {
		return fmt.Errorf("check default state root: %w", err)
	}
	if os.SameFile(selected, standard) && tail == defaultTail {
		return errDefaultState
	}
	return nil
}

// existingAncestor returns the identity of the deepest existing path and the
// names still to be created below it. A dangling link is an error, not a
// missing directory that the launcher can safely create.
func existingAncestor(path string) (fs.FileInfo, string, error) {
	path = filepath.Clean(path)
	var tail string
	for {
		info, err := os.Lstat(path)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				info, err = os.Stat(path)
			}
			return info, tail, err
		}
		parent := filepath.Dir(path)
		if !errors.Is(err, fs.ErrNotExist) || parent == path {
			return nil, "", err
		}
		tail = filepath.Join(filepath.Base(path), tail)
		path = parent
	}
}
