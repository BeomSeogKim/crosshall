package ident

import (
	"errors"
	"io/fs"
	"os"
	"testing"
)

// A restart between a commit and its publication leaves the old generation
// and cursor on disk; republishing installs the committed generation without
// the old cursor, and again changes nothing.
func TestRepublishGenerationDropsTheOldCursorOnce(t *testing.T) {
	dir := privateTempDir(t)
	leaf, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer leaf.Close()
	for name, value := range map[string]string{"gen": "old", "ack": "999999\n", "rules": "999999\n"} {
		if err := os.WriteFile(dir+"/"+name, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := RepublishGeneration(leaf, "new"); err != nil {
		t.Fatal(err)
	}
	if gen, err := ReadPrivateFile(leaf, "gen", 64); err != nil || string(gen) != "new" {
		t.Fatalf("gen %q (%v), want new", gen, err)
	}
	for _, receipt := range []string{"ack", "rules"} {
		if _, err := os.Stat(dir + "/" + receipt); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("the old %s cursor survived: %v", receipt, err)
		}
	}
	if err := os.WriteFile(dir+"/ack", []byte("7\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := RepublishGeneration(leaf, "new"); err != nil {
		t.Fatal(err)
	}
	if ack, err := os.ReadFile(dir + "/ack"); err != nil || string(ack) != "7\n" {
		t.Fatalf("a repeated republish touched the current cursor: %q (%v)", ack, err)
	}
}
