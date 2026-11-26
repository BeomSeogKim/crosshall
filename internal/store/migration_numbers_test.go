package store

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The migrator is positional: user_version is the number of entries
// applied. Each file must be numbered by its place in the slice, with no
// gap, or a later file inserted into a gap would never run.
func TestMigrationFilesAreNumberedInSliceOrder(t *testing.T) {
	for _, schema := range []struct {
		dir     string
		entries []string
	}{
		{"migrations", migrations},
		{"migrations/spoke", spokeMigrations},
	} {
		t.Run(schema.dir, func(t *testing.T) {
			files, err := filepath.Glob(filepath.Join(schema.dir, "*.sql"))
			if err != nil {
				t.Fatal(err)
			}
			if len(files) != len(schema.entries) {
				t.Fatalf("%d migration files, %d in the slice", len(files), len(schema.entries))
			}
			for i, file := range files {
				if want := fmt.Sprintf("%03d_", i+1); !strings.HasPrefix(filepath.Base(file), want) {
					t.Errorf("entry %d is %s, want a name starting %s", i+1, filepath.Base(file), want)
				}
				body, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				if string(body) != schema.entries[i] {
					t.Errorf("entry %d of the slice is not %s", i+1, filepath.Base(file))
				}
			}
		})
	}
}
