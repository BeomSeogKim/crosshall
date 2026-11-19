//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestSetupCodexPinsGeneratedHooksToTheirStateRoot(t *testing.T) {
	selected := newSetupWorld(t)
	selected.state = filepath.Join(selected.home, "owner's selected state")
	envWithState := func(root string) []string {
		env := slices.DeleteFunc(slices.Clone(selected.env), func(entry string) bool {
			return strings.HasPrefix(entry, "CROSSHALL_STATE_DIR=")
		})
		return append(env, "CROSSHALL_STATE_DIR="+root)
	}
	selected.env = envWithState(selected.state)
	selected.apply("", "setup", "codex", "--apply")

	// Run the generated shell command with a probe at the installed executable
	// path. Routing depends on the shell assignment and quoting. Native hook
	// output also depends on finishing within its callback deadline.
	probe := selected.executable + "-probe"
	const script = "#!/bin/sh\nprintf '%s\\n' \"$0\" \"$CROSSHALL_STATE_DIR\" \"$@\"\n"
	if err := os.WriteFile(probe, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(probe, selected.executable); err != nil {
		t.Fatal(err)
	}
	otherRoot := filepath.Join(t.TempDir(), "other state")
	config := readSetupFile(t, selected.paths["codex"])
	for _, event := range []string{"SessionStart", "UserPromptSubmit", "Stop", "PermissionRequest", "SessionEnd"} {
		t.Run(event, func(t *testing.T) {
			hook := setupHookFor(t, config, event)
			if len(hook.Args) != 0 || hook.Timeout != 1 {
				t.Fatalf("setup changed the native hook options: %+v", hook)
			}
			want := strings.Join([]string{selected.executable, selected.state, "hook", "codex", event, ""}, "\n")
			for _, root := range []string{otherRoot, selected.state} {
				cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", hook.Command)
				cmd.Dir, cmd.Env = selected.home, envWithState(root)
				if out, err := cmd.CombinedOutput(); err != nil || string(out) != want {
					t.Errorf("hook invoked with inherited state %q: %v, got %q, want %q", root, err, out, want)
				}
			}
		})
	}
}
