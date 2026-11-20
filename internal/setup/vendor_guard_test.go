//go:build testvendorhome

package setup

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestVendorSentinelRecordsIgnoredConfigReads(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	claude := filepath.Join(root, ".claude")
	codex := filepath.Join(root, "custom codex")
	agy := filepath.Join(root, ".gemini", "config")
	for _, dir := range []string{claude, codex, agy, codex + "-fixture"} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	settings := filepath.Join(claude, "settings.json")
	hooks := filepath.Join(codex, "hooks.json")
	agyHooks := filepath.Join(agy, "hooks.json")
	allowed := filepath.Join(codex+"-fixture", "hooks.json")
	for _, path := range []string{settings, hooks, agyHooks, allowed} {
		if err := os.WriteFile(path, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	sentinel := t.TempDir()
	watchedClaude := claude
	if runtime.GOOS == "darwin" && (strings.HasPrefix(claude, "/private/tmp/") || strings.HasPrefix(claude, "/private/var/")) {
		// The Darwin system aliases are also allowed by the configuration guard.
		watchedClaude = strings.TrimPrefix(claude, "/private")
	}
	protected := strings.Join([]string{watchedClaude, codex, agy}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(sentinel, "protected"), []byte(protected), 0600); err != nil {
		t.Fatal(err)
	}

	t.Setenv(testVendorSentinel, sentinel)
	t.Setenv("HOME", root)
	t.Setenv("CLAUDE_CONFIG_DIR", claude)
	t.Setenv("CODEX_HOME", codex)
	t.Setenv("PATH", t.TempDir())

	// Inspect suppresses configuration read errors for owner doctor, but CI
	// still needs a durable record of the synthetic violation.
	statuses := Inspect(filepath.Join(root, "missing-state"))
	for _, status := range statuses {
		if status.InstallState != unknown {
			t.Fatalf("protected fixture was inspected: %+v", status)
		}
	}
	log, err := os.ReadFile(filepath.Join(sentinel, "violations"))
	var want strings.Builder
	for _, path := range []string{settings, hooks, agyHooks} {
		fmt.Fprintf(&want, "protected vendor path accessed: %q\n", path)
	}
	if err != nil || string(log) != want.String() {
		t.Fatalf("guard log = %q, %v; want %q", log, err, want.String())
	}
	if contents, err := readConfig(allowed); err != nil || string(contents.Bytes) != "{}" {
		t.Fatalf("read from sibling fixture = %q, %v", contents.Bytes, err)
	}
	linked := filepath.Join(root, "linked-claude")
	if err := os.Symlink(claude, linked); err != nil {
		t.Fatal(err)
	}
	if contents, err := readConfig(filepath.Join(linked, "settings.json")); err == nil || contents.Exists {
		t.Fatalf("read through the symlink = %+v, %v", contents, err)
	}
	if contents, err := readConfig(hooks); !errors.Is(err, errTestVendorPath) || contents.Exists {
		t.Fatalf("direct protected read = %+v, %v", contents, err)
	}
}
