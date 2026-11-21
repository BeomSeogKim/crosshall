//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func gitRepository(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

// linkedWorktree lays out what `git worktree add` leaves: a .git file in the
// worktree and a directory under the repository's .git/worktrees.
func linkedWorktree(t *testing.T, repo, dir string) string {
	t.Helper()
	admin := filepath.Join(repo, ".git", "worktrees", filepath.Base(dir))
	for _, d := range []string{admin, dir} {
		if err := os.MkdirAll(d, 0700); err != nil {
			t.Fatal(err)
		}
	}
	for path, text := range map[string]string{
		filepath.Join(admin, "HEAD"):      "ref: refs/heads/side\n",
		filepath.Join(admin, "commondir"): "../..\n",
		filepath.Join(dir, ".git"):        "gitdir: " + admin + "\n",
	} {
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// With the owner's switch on, a session that starts in a Git repository
// with no channel gets the repository's own channel, and a session in a
// linked worktree of that repository joins the same one.
func TestAutomaticJoiningGivesARepositoryItsChannel(t *testing.T) {
	f := newSetupWorld(t)
	f.apply("", "setup", "claude", "codex", "--apply")
	owner := f.owner()
	if got := owner.must("owner", "channel", "auto", "on"); !strings.Contains(got.stdout, `"on":true`) {
		t.Fatalf("switch on: %s", got.stdout)
	}
	repo := gitRepository(t, filepath.Join(f.home, "Shop API"))
	worktree := linkedWorktree(t, repo, filepath.Join(f.home, "shop-side"))

	if got := f.invokeHook(f.home, "claude", "SessionStart", `"source":"startup"`); got != "" {
		t.Fatalf("a start outside Git printed %q", got)
	}
	if got := startAs(f, "claude", "one", repo); !strings.Contains(got, "Joined #shop-api as @claude1") {
		t.Fatalf("first start in the repository printed %q", got)
	}
	if got := startAs(f, "codex", "two", worktree); !strings.Contains(got, "Joined #shop-api as @codex1") {
		t.Fatalf("start in a linked worktree printed %q", got)
	}
	var channels, defaults int
	var root string
	if err := f.db().QueryRow(`SELECT (SELECT COUNT(*) FROM channel), (SELECT COUNT(*) FROM repo_default),
		(SELECT repo_root FROM repo_default)`).Scan(&channels, &defaults, &root); err != nil || channels != 1 || defaults != 1 ||
		filepath.Base(root) != "Shop API" {
		t.Fatalf("channels %d, defaults %d, root %q (%v)", channels, defaults, root, err)
	}
}

// The owner's explicit default for a worktree wins over the repository's
// channel, a second repository of the same name gets a suffix, and with the
// switch off again nothing is created while a bound repository keeps its channel.
func TestAutomaticJoiningKeepsOwnerChoices(t *testing.T) {
	f := newSetupWorld(t)
	f.apply("", "setup", "claude", "codex", "--apply")
	owner := f.owner()
	owner.must("owner", "channel", "auto", "on")
	owner.must("owner", "channel", "create", "api")
	repo := gitRepository(t, filepath.Join(f.home, "one", "api"))
	worktree := linkedWorktree(t, repo, filepath.Join(f.home, "review"))
	owner.must("owner", "channel", "create", "review-team")
	owner.must("owner", "channel", "default", "review-team", "--repo", worktree)

	if got := startAs(f, "claude", "one", repo); !strings.Contains(got, "Joined #api-2 as @claude1") {
		t.Fatalf("a repository whose name is taken printed %q", got)
	}
	if got := startAs(f, "codex", "two", worktree); !strings.Contains(got, "Joined #review-team as @codex1") {
		t.Fatalf("a worktree with its own default printed %q", got)
	}
	owner.must("owner", "channel", "auto", "off")
	fresh := gitRepository(t, filepath.Join(f.home, "fresh"))
	if got := f.invokeHook(fresh, "codex", "SessionStart", `"source":"startup"`); strings.Contains(got, "Joined #") {
		t.Fatalf("with the switch off a start in an unbound repository printed %q", got)
	}
	if got := startAs(f, "codex", "third", repo); !strings.Contains(got, "Joined #api-2 as @codex") {
		t.Fatalf("with the switch off a start in a bound repository printed %q", got)
	}
	var channels int
	if err := f.db().QueryRow(`SELECT COUNT(*) FROM channel`).Scan(&channels); err != nil || channels != 3 {
		t.Fatalf("channels %d (%v)", channels, err)
	}
}

// startAs starts a session of the adapter in dir and returns the welcome it
// was given: at the start, or at its first prompt when a loaded machine
// took the start past its deadline (the welcome stays pending until read).
func startAs(f *setupWorld, adapter, session, dir string) string {
	f.t.Helper()
	fire := func(event, fields string) string {
		cmd := f.installedHookCommand(f.t.Context(), dir, adapter, event, fields)
		cmd.Stdin = strings.NewReader(fmt.Sprintf(`{"hook_event_name":%q,"session_id":%q,"cwd":%q,%s}`, event, session, dir, fields))
		out, err := cmd.Output()
		if err != nil {
			f.t.Fatalf("%s %s of session %s: %v", adapter, event, session, err)
		}
		return string(out)
	}
	if out := fire("SessionStart", `"source":"startup"`); out != "" {
		return out
	}
	return fire("UserPromptSubmit", `"prompt_id":"first","turn_id":"first","prompt":"synthetic"`)
}
