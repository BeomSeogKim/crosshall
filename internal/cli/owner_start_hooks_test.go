package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

// The explicit owner start is where an upgrading owner is told that the
// installed hooks are older than the binary: the sentences follow the
// started line. The JSON result stays the one object it was.
func TestOwnerStartPrintsOutdatedHooksAfterTheStartedLine(t *testing.T) {
	sentences := []string{
		"The Claude Code hooks on disk were installed for claude-2; this crosshall installs claude-3. Run crosshall setup claude.",
		"The Codex hooks on disk were installed for codex-1; this crosshall installs codex-2. Run crosshall setup codex.",
	}
	env := Env{
		Start:         func(context.Context) error { return nil },
		OutdatedHooks: func() []string { return sentences },
	}
	stdout, stderr, code := run(t, env, "owner", "start")
	if want := "dispatcher running\n" + strings.Join(sentences, "\n") + "\n"; code != core.ExitOK || stdout != want {
		t.Fatalf("exit %d, stdout %q, stderr %q; want %q", code, stdout, stderr, want)
	}
	stdout, stderr, code = run(t, env, "--json", "owner", "start")
	if want := `{"v":1,"ok":true,"kind":"owner start"}` + "\n"; code != core.ExitOK || stdout != want {
		t.Fatalf("JSON: exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	env.OutdatedHooks = func() []string { return nil }
	if stdout, _, _ = run(t, env, "owner", "start"); stdout != "dispatcher running\n" {
		t.Fatalf("with current hooks: %q", stdout)
	}
}
