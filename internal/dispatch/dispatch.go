// Package dispatch defines dispatcher startup checks and command handoff.
package dispatch

import (
	"errors"
	"path/filepath"
)

// CheckStateRoot requires development and test builds to use an explicit state
// root other than the default. Paths must already be resolved to their
// canonical locations.
func CheckStateRoot(version, stateRoot, defaultStateRoot string) error {
	switch version {
	case "", "dev", "test":
		if stateRoot == "" || filepath.Clean(stateRoot) == filepath.Clean(defaultStateRoot) {
			return errors.New("development build requires CROSSHALL_STATE_DIR outside the default state root")
		}
	}
	return nil
}
