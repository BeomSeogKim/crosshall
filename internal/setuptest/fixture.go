// Package setuptest isolates adapter setup tests from the user's home and CLIs.
package setuptest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The crosshall skill setup installs under each vendor's home.
const (
	skillDir  = "skills/crosshall"
	skillFile = skillDir + "/SKILL.md"
)

type Fixture struct {
	Root, Home, State, Bin string
	Outside                string
}

// New selects a disposable environment and puts only the requested fake vendor
// executables on PATH. Vendor overrides are initially absent to exercise defaults.
func New(t testing.TB, vendors ...string) Fixture {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(parent, "fixture")
	f := Fixture{
		Root: root, Home: filepath.Join(root, "home"), State: filepath.Join(root, "state"),
		Bin: filepath.Join(root, "bin"), Outside: filepath.Join(parent, "outside"),
	}
	for _, dir := range []string{f.Home, f.Bin, filepath.Join(f.Outside, ".claude", skillDir),
		filepath.Join(f.Outside, ".codex", skillDir), filepath.Join(f.Outside, ".gemini", "config", skillDir)} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{".claude/settings.json", ".codex/hooks.json", ".gemini/config/hooks.json",
		".claude/" + skillFile, ".codex/" + skillFile, ".gemini/config/" + skillFile} {
		path := filepath.Join(f.Outside, name)
		const marker = "{\"outside_fixture\":true}\n"
		if err := os.WriteFile(path, []byte(marker), 0600); err != nil {
			t.Fatal(err)
		}
		before, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			info, err := os.Lstat(path)
			if err != nil || !os.SameFile(before, info) || info.Mode() != before.Mode() || !info.ModTime().Equal(before.ModTime()) {
				t.Errorf("outside sentinel changed: %s", path)
				return
			}
			if data, err := os.ReadFile(path); err != nil || string(data) != marker {
				t.Errorf("outside sentinel contents changed: %s", path)
			}
		})
	}
	t.Setenv("HOME", f.Home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "xdg"))
	t.Setenv("CROSSHALL_STATE_DIR", f.State)
	t.Setenv("PATH", f.Bin)
	for _, name := range []string{"CLAUDE_CONFIG_DIR", "CODEX_HOME"} {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
	f.CheckTargets(t)
	for _, name := range vendors {
		var version string
		switch name {
		case "claude":
			version = "2.1.327 (Claude Code)"
		case "codex":
			version = "codex-cli 0.170.0"
		case "agy":
			version = "1.5.13"
		default:
			t.Fatalf("unknown fixture vendor %q", name)
		}
		program := "#!/bin/sh\n[ \"$1\" = --version ] || exit 2\nprintf '%s\\n' '" + version + "'\n"
		if err := os.WriteFile(filepath.Join(f.Bin, name), []byte(program), 0700); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

// CheckTargets must run immediately before a write-capable setup invocation,
// after the test has selected its overrides. It checks file targets as well as
// their parents, even if the final file or directory has yet to be created.
func (f Fixture) CheckTargets(t testing.TB) {
	t.Helper()
	home := os.Getenv("HOME")
	agy := filepath.Join(home, ".gemini", "config")
	paths := []string{home, os.Getenv("CROSSHALL_STATE_DIR"), os.Getenv("XDG_STATE_HOME"),
		filepath.Join(agy, "hooks.json"), filepath.Join(agy, skillDir), filepath.Join(agy, skillFile)}
	for _, target := range []struct{ variable, dir, file string }{
		{"CLAUDE_CONFIG_DIR", ".claude", "settings.json"}, {"CODEX_HOME", ".codex", "hooks.json"},
	} {
		dir, ok := os.LookupEnv(target.variable)
		if !ok {
			dir = filepath.Join(home, target.dir)
		}
		paths = append(paths, dir, filepath.Join(dir, target.file), filepath.Join(dir, skillDir), filepath.Join(dir, skillFile))
	}
	for _, path := range paths {
		if err := f.checkPath(path); err != nil {
			t.Fatal(err)
		}
	}
}

func (f Fixture) checkPath(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("invalid fixture path %q", path)
	}
	// Require a lexical child and inspect each existing component before going
	// deeper. A link cannot redirect the check into someone's real settings.
	rel, err := filepath.Rel(f.Root, path)
	if err != nil || !filepath.IsLocal(rel) {
		return fmt.Errorf("path outside fixture: %s", path)
	}
	next := f.Root
	for _, part := range append([]string{"."}, strings.Split(rel, string(os.PathSeparator))...) {
		next = filepath.Join(next, part)
		fi, err := os.Lstat(next)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil || fi.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe fixture path component: %s", next)
		}
	}
	return nil
}
