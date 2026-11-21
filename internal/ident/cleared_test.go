package ident

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

// A renderer holding the directory keeps the rotation out until it is done;
// the dispatcher's next pass rotates.
func TestRepublishGenerationWaitsForARenderer(t *testing.T) {
	dir := privateTempDir(t)
	leaf, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer leaf.Close()
	if err := os.WriteFile(dir+"/gen", []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Flock(int(held.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	if err := RepublishGeneration(leaf, "new"); !errors.Is(err, ErrDirectoryBusy) {
		t.Fatalf("rotation under a renderer's lock: %v, want busy", err)
	}
	if gen, _ := ReadPrivateFile(leaf, "gen", 64); string(gen) != "old" {
		t.Fatalf("a busy rotation changed gen to %q", gen)
	}
	held.Close()
	if err := RepublishGeneration(leaf, "new"); err != nil {
		t.Fatal(err)
	}
}

// Astra #732: a hello registration that publishes a new generation goes the
// same way as a rotation: the old membership's cursors go with it.
func TestHelloPublicationDropsTheOldCursors(t *testing.T) {
	root := privateTempDir(t)
	digest := strings.Repeat("ab", 32)
	dir := filepath.Join(root, "m", digest[:16])
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"gen": "aabbccddaabbccddaabbccddaabbccdd", "ack": "999999\n", "rules": "999999\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	leaf, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	fileID, err := FileIdentity(leaf)
	leaf.Close()
	if err != nil {
		t.Fatal(err)
	}
	reg := Registration{rendezvous: root, digest: digest, fileID: fileID, generation: "11223344112233441122334411223344"}
	if err := reg.Publish(); err != nil {
		t.Fatal(err)
	}
	for _, receipt := range []string{"ack", "rules"} {
		if _, err := os.Stat(filepath.Join(dir, receipt)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("hello left the old %s cursor under the new generation: %v", receipt, err)
		}
	}
	if gen, err := os.ReadFile(filepath.Join(dir, "gen")); err != nil || strings.TrimSpace(string(gen)) != reg.generation {
		t.Fatalf("gen %q (%v)", gen, err)
	}
}
