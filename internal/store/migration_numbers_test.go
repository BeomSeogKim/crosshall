package store

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// The migrator is positional: user_version is the number of entries
// applied. Each file must be numbered by its place in the slice, with no
// gap, or a later file inserted into a gap would never run.
func TestMigrationFilesAreNumberedInSliceOrder(t *testing.T) {
	files, err := filepath.Glob("migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != len(migrations) {
		t.Fatalf("%d migration files, %d in the slice", len(files), len(migrations))
	}
	for i, file := range files { // Glob sorts by name
		if want := fmt.Sprintf("%03d_", i+1); filepath.Base(file)[:4] != want {
			t.Errorf("entry %d is %s, want a name starting %s", i+1, filepath.Base(file), want)
		}
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != migrations[i] {
			t.Errorf("entry %d of the slice is not %s", i+1, filepath.Base(file))
		}
	}
}
