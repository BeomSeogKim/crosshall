package spool

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadServerPacketRejectsInRootLink(t *testing.T) {
	path := t.TempDir()
	root, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if err := os.WriteFile(filepath.Join(path, "target"), []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(path, "packet.json")); err != nil {
		t.Fatal(err)
	}
	if data, err := readServerPacket(root, "packet.json"); err == nil {
		t.Fatalf("server read a linked packet: %q", data)
	}
}
