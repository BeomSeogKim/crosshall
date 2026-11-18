package dispatch

import (
	"path/filepath"
	"testing"
)

func TestCheckStateRoot(t *testing.T) {
	root := t.TempDir()
	defaultRoot := filepath.Join(root, "default")
	separateRoot := filepath.Join(root, "sandbox")
	for _, tc := range []struct {
		name      string
		version   string
		stateRoot string
		wantError bool
	}{
		{"development without override", "dev", "", true},
		{"development with default override", "dev", defaultRoot, true},
		{"development with separate override", "dev", separateRoot, false},
		{"test with default override", "test", defaultRoot, true},
		{"release with default root", "v0.1.0", defaultRoot, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckStateRoot(tc.version, tc.stateRoot, defaultRoot)
			if (err != nil) != tc.wantError {
				t.Fatalf("CheckStateRoot returned %v, want error: %t", err, tc.wantError)
			}
		})
	}
}
