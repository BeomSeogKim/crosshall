//go:build testvendorhome

package setup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const testVendorSentinel = "CROSSHALL_TEST_VENDOR_SENTINEL"

var errTestVendorPath = errors.New("vendor path is protected during tests")

// The CI fixture guards the setup file boundary before any configuration
// directory is opened. Log attempts even if a caller suppresses the error.
func checkTestVendorPath(path string) error {
	sentinel := os.Getenv(testVendorSentinel)
	if sentinel == "" {
		return nil
	}
	protected, err := os.ReadFile(filepath.Join(sentinel, "protected"))
	if err != nil {
		panic(fmt.Errorf("read test vendor sentinel: %w", err))
	}
	target := testVendorPath(path)
	for _, dir := range strings.Split(string(protected), "\n") {
		if dir == "" {
			continue
		}
		relative, err := filepath.Rel(testVendorPath(dir), target)
		if err == nil && filepath.IsLocal(relative) {
			recordTestVendorAccess(sentinel, path)
			return errTestVendorPath
		}
	}
	return nil
}

func testVendorPath(path string) string {
	path = systemConfigDirectory(filepath.Clean(path))
	if runtime.GOOS == "darwin" {
		path = strings.ToLower(path)
	}
	return path
}

func recordTestVendorAccess(sentinel, path string) {
	log, err := os.OpenFile(filepath.Join(sentinel, "violations"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		panic(fmt.Errorf("open test vendor violation log: %w", err))
	}
	_, writeErr := fmt.Fprintf(log, "protected vendor path accessed: %q\n", path)
	if err := errors.Join(writeErr, log.Close()); err != nil {
		panic(fmt.Errorf("record test vendor violation: %w", err))
	}
}
