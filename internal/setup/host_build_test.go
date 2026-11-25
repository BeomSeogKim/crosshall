package setup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReprobeReadsTheRecordedExecutableAgain(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "agy")
	write := func(version string) {
		if err := os.WriteFile(exe, []byte("#!/bin/sh\necho "+version+"\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	write("1.5.16")
	first, ok := Reprobe(context.Background(), "agy", HostBuild{Executable: exe}, 2*time.Second)
	if !ok || first.Version != "1.5.16" || !first.Current() {
		t.Fatalf("first probe: %+v %t", first, ok)
	}
	write("1.5.17 ")
	if err := os.Chtimes(exe, time.Now(), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if first.Current() {
		t.Fatal("a changed file is still the recorded build")
	}
	second, ok := Reprobe(context.Background(), "agy", first, 2*time.Second)
	if !ok || second.Version != "1.5.17" || second.Executable != exe || !second.Current() {
		t.Fatalf("after the update: %+v %t", second, ok)
	}
	write("not a version")
	if b, ok := Reprobe(context.Background(), "agy", second, 2*time.Second); ok {
		t.Fatalf("unparsed version accepted: %+v", b)
	}
	if b, ok := Reprobe(context.Background(), "agy", HostBuild{Executable: exe + "-gone"}, 2*time.Second); ok {
		t.Fatalf("missing executable: %+v", b)
	}
}
