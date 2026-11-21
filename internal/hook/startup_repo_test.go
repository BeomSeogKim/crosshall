package hook

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/ident"
)

func TestStartupRepoMarkerSurvivesMissingRendezvousAndDuplicates(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		for _, name := range []string{"CODEX_THREAD_ID", "ANTIGRAVITY_CONVERSATION_ID"} {
			t.Setenv(name, "")
			if err := os.Unsetenv(name); err != nil {
				t.Fatal(err)
			}
		}
		state := t.TempDir()
		if err := os.Chmod(state, 0700); err != nil {
			t.Fatal(err)
		}
		repo, err := filepath.EvalSymlinks(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(repo, ".git"), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0600); err != nil {
			t.Fatal(err)
		}
		nested := filepath.Join(repo, "nested")
		if err := os.Mkdir(nested, 0700); err != nil {
			t.Fatal(err)
		}
		var attempts atomic.Int32
		paths := Paths{
			State: state, Rendezvous: filepath.Join(state, "rendezvous"),
			Start: func(context.Context) (bool, error) {
				attempts.Add(1)
				return false, nil
			},
		}
		locate := func() (Paths, error) { return paths, nil }
		fire := func(entry time.Time, id ident.Identity, source, cwd string) {
			t.Helper()
			t.Setenv("CLAUDE_CODE_SESSION_ID", id.Key)
			body := fmt.Sprintf(`{"hook_event_name":"SessionStart","session_id":%q,"source":%q,"cwd":%q}`, id.Key, source, cwd)
			Run(entry, []string{"claude", "SessionStart"}, strings.NewReader(body), io.Discard, locate)
		}
		outside := ident.Identity{Vendor: "claude", Key: "opened-outside"}
		fire(time.Now(), outside, "startup", state)
		fire(time.Now(), outside, "resume", repo)
		if got := startupRepoDigest(state, outside); got != "-\n" {
			t.Fatalf("duplicate startup replaced the outside marker: %q", got)
		}
		inside := ident.Identity{Vendor: "claude", Key: "opened-in-project"}
		fire(time.Now(), inside, "resume", nested)
		if got := startupRepoDigest(state, inside); got != repoDigestRecord(repo) || strings.Contains(got, repo) {
			t.Fatalf("resume without policy or rendezvous did not leave only its canonical repo digest: %q", got)
		}
		if err := os.WriteFile(filepath.Join(state, StatePolicyName), []byte("claude 1 0 1\n"), 0600); err != nil {
			t.Fatal(err)
		}
		for _, source := range []string{"clear", "compact"} {
			id := ident.Identity{Vendor: "claude", Key: source}
			fire(time.Now().Add(-2*Budget), id, source, nested)
			if got := startupRepoDigest(state, id); got != repoDigestRecord(repo) {
				t.Fatalf("late %s callback left startup marker %q", source, got)
			}
		}
		if attempts.Load() != 0 {
			t.Fatalf("recording without policy or after deadline attempted to start %d dispatchers", attempts.Load())
		}
		if err := os.Remove(filepath.Join(state, StatePolicyName)); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(paths.Rendezvous, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(paths.Rendezvous, "repo-defaults"), []byte(repoDigestRecord(repo)), 0600); err != nil {
			t.Fatal(err)
		}
		rv, err := ident.OpenPrivateDir(paths.Rendezvous)
		if err != nil {
			t.Fatal(err)
		}
		defer rv.Close()
		if got := mappedEnrollmentRoot(t.Context(), rv, state, event{Identity: inside, CWD: repo}); got.worktree != repo || got.repository != repo || got.create {
			t.Fatalf("same canonical repository was refused: %+v", got)
		}
		if got := mappedEnrollmentRoot(t.Context(), rv, state, event{Identity: outside, CWD: repo}); !got.none() {
			t.Fatalf("session opened outside was allowed to recover in %+v", got)
		}
		incomplete := ident.Identity{Vendor: "claude", Key: "incomplete"}
		if err := os.WriteFile(filepath.Join(state, startupRepoDir, incomplete.Digest()), []byte("1"), 0600); err != nil {
			t.Fatal(err)
		}
		if got := mappedEnrollmentRoot(t.Context(), rv, state, event{Identity: incomplete, CWD: repo}); !got.none() {
			t.Fatalf("session with incomplete marker was allowed to recover in %+v", got)
		}
		if err := os.Remove(filepath.Join(paths.Rendezvous, "repo-defaults")); err != nil {
			t.Fatal(err)
		}
		rv.Close()
		if err := os.Remove(paths.Rendezvous); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CLAUDE_CODE_SESSION_ID", inside.Key)
		body := fmt.Sprintf(`{"hook_event_name":"SessionEnd","session_id":%q,"reason":"other"}`, inside.Key)
		Run(time.Now(), []string{"claude", "SessionEnd"}, strings.NewReader(body), io.Discard, locate)
		if got := startupRepoDigest(state, inside); got != "" {
			t.Fatalf("ending a session without a policy or rendezvous left its startup marker %q", got)
		}
		if attempts.Load() != 0 {
			t.Fatalf("record removal attempted to start %d dispatchers", attempts.Load())
		}
	})
}

// A session joins only where its first start was. A record written before
// repositories had one identity names the worktree, and still counts there.
func TestStartRecordNamesTheRepositoryOrALegacyWorktree(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(base, "state")
	repo, linked, other := filepath.Join(base, "repo"), filepath.Join(base, "linked"), filepath.Join(base, "other")
	admin := filepath.Join(repo, ".git", "worktrees", "linked")
	for path, text := range map[string]string{
		filepath.Join(repo, ".git", "HEAD"):  "ref: refs/heads/main\n",
		filepath.Join(admin, "HEAD"):         "ref: refs/heads/side\n",
		filepath.Join(admin, "commondir"):    "../..\n",
		filepath.Join(linked, ".git"):        "gitdir: " + admin + "\n",
		filepath.Join(other, ".git", "HEAD"): "ref: refs/heads/main\n",
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(state, startupRepoDir), 0700); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		record        string
		later, prompt bool
	}{
		"none":             {"", true, false},
		"repository":       {repoDigestRecord(repo), true, true},
		"legacy worktree":  {repoDigestRecord(linked), true, true},
		"another place":    {repoDigestRecord(other), false, false},
		"outside Git":      {repoDigestRecord(""), false, false},
		"incomplete write": {"1", false, false},
	} {
		id := ident.Identity{Vendor: "claude", Key: name}
		if c.record != "" {
			if err := os.WriteFile(filepath.Join(state, startupRepoDir, id.Digest()), []byte(c.record), 0600); err != nil {
				t.Fatal(err)
			}
		}
		e := event{Identity: id, CWD: linked}
		if got := startedHere(state, e, false); got != c.later {
			t.Errorf("%s: a later start in the linked worktree: %v", name, got)
		}
		if got := startedHere(state, e, true); got != c.prompt {
			t.Errorf("%s: a prompt in the linked worktree: %v", name, got)
		}
	}
}
