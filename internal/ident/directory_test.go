package ident

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateDirectoryAndFiles(t *testing.T) {
	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := OpenPrivateDir(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	if err := dir.Mkdir("leaf", 0700); err != nil {
		t.Fatal(err)
	}
	if err := dir.Symlink("leaf", "alias"); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenPrivateChild(dir, "alias"); err == nil {
		t.Fatal("accepted a linked directory")
	}
	leaf, err := OpenPrivateChild(dir, "leaf")
	if err != nil {
		t.Fatal(err)
	}
	defer leaf.Close()
	if err := leaf.WriteFile("gen", []byte("value"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := leaf.Symlink("gen", "linked"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"linked", "../leaf/gen", "gen/sub"} {
		if _, err := ReadPrivateFile(leaf, name, 5); err == nil {
			t.Errorf("accepted %q", name)
		}
	}
	if _, err := ReadPrivateFile(leaf, "gen", 4); err == nil {
		t.Fatal("short limit truncated metadata")
	}
	if err := dir.Rename("leaf", "moved"); err != nil {
		t.Fatal(err)
	}
	got, err := ReadPrivateFile(leaf, "gen", 5)
	if err != nil || string(got) != "value" {
		t.Fatalf("pinned directory returned %q, %v", got, err)
	}
	if err := leaf.Chmod("gen", 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadPrivateFile(leaf, "gen", 5); err == nil {
		t.Fatal("accepted non-private metadata")
	}
}

func TestRegistrationPublishesOnlyToRegisteredLeaf(t *testing.T) {
	path := t.TempDir()
	if err := os.Chmod(path, 0700); err != nil {
		t.Fatal(err)
	}
	digest := Identity{Vendor: "claude", Key: "one"}.Digest()
	leafPath := filepath.Join(path, "m", digest[:16])
	if err := os.MkdirAll(leafPath, 0700); err != nil {
		t.Fatal(err)
	}
	leaf, err := OpenPrivateDir(leafPath)
	if err != nil {
		t.Fatal(err)
	}
	defer leaf.Close()
	fileID, err := FileIdentity(leaf)
	if err != nil {
		t.Fatal(err)
	}
	reg := Registration{rendezvous: path, digest: digest, fileID: fileID, generation: "aabbccddaabbccddaabbccddaabbccdd"}
	if err := reg.Publish(); err != nil {
		t.Fatal(err)
	}
	first, err := leaf.Stat("gen")
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Publish(); err != nil {
		t.Fatal(err)
	}
	next, err := leaf.Stat("gen")
	if err != nil || !os.SameFile(first, next) {
		t.Fatalf("unchanged generation was replaced: %v", err)
	}
	if err := os.Rename(leafPath, leafPath+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(leafPath, 0700); err != nil {
		t.Fatal(err)
	}
	if err := reg.Publish(); err == nil {
		t.Fatal("published to a replaced leaf")
	}
	if _, err := os.Lstat(filepath.Join(leafPath, "gen")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("replacement has a generation: %v", err)
	}
}
