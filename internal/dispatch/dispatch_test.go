package dispatch

import (
	"os"
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

func TestCheckStateRootPhysicalDirectory(t *testing.T) {
	home := t.TempDir()
	alias := filepath.Join(t.TempDir(), "home")
	if err := os.Symlink(home, alias); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(home, "state", "crosshall")
	throughAlias := filepath.Join(alias, "state", "crosshall")
	t.Setenv("XDG_STATE_HOME", filepath.Dir(throughAlias))
	for _, existing := range []bool{false, true} {
		if existing {
			if err := os.MkdirAll(root, 0700); err != nil {
				t.Fatal(err)
			}
		}
		if CheckStateRoot("dev", throughAlias, root) == nil || CheckStateRoot("test", root, throughAlias) == nil {
			t.Errorf("the same physical root was allowed (already exists: %v)", existing)
		}
		if CheckStateRoot("dev", root, "") == nil {
			t.Errorf("implicit XDG default was allowed (already exists: %v)", existing)
		}
		if err := CheckStateRoot("dev", filepath.Join(alias, "state", "other"), root); err != nil {
			t.Errorf("separate sibling refused: %v", err)
		}
	}
}
