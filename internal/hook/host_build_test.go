package hook

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTheHostBuildIsTheRecordedFileOnly(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(dir, "agy")
	if err := os.WriteFile(exe, []byte("binary"), 0700); err != nil {
		t.Fatal(err)
	}
	rv, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer rv.Close()
	args := []string{"agy"}
	saved := hostCommand
	hostCommand = func() (int, string, []string, bool) { return 1, exe, args, true }
	defer func() { hostCommand = saved }()
	m := manifest{Adapter: "agy", BuildFromHost: true, HostModeDefault: "interactive",
		HostModes: map[string][]string{"print": {"-p", "--print"}}}
	record := func(version string, size int64, path string) {
		info, err := os.Stat(exe)
		if err != nil {
			t.Fatal(err)
		}
		line := HostBuildLine("agy", version, FileID(info), size, info.ModTime().UnixNano(), path)
		if err := os.WriteFile(filepath.Join(dir, HostBuildsName), []byte(line), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if v, mode := hostBuild(rv, m); v != "unverified" || mode != "interactive" {
		t.Fatalf("no record: %s %s", v, mode)
	}
	record("1.5.16", 6, realPath(exe))
	if v, mode := hostBuild(rv, m); v != "1.5.16" || mode != "interactive" {
		t.Fatalf("recorded: %s %s", v, mode)
	}
	args = []string{"agy", "-p", "hello"}
	if v, mode := hostBuild(rv, m); v != "1.5.16" || mode != "print" {
		t.Fatalf("print: %s %s", v, mode)
	}
	for name, change := range map[string]func(){
		"another size":  func() { record("1.5.16", 7, realPath(exe)) },
		"another path":  func() { record("1.5.16", 6, realPath(exe)+"2") },
		"not a version": func() { record("cli-1.5.16", 6, realPath(exe)) },
		"another adapter": func() {
			os.WriteFile(filepath.Join(dir, HostBuildsName), []byte("claude 1.5.16 0:0 6 0 "+exe+"\n"), 0600)
		},
	} {
		change()
		if v, _ := hostBuild(rv, m); v != "unverified" {
			t.Errorf("%s: %s", name, v)
		}
	}
}
