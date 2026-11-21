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
	"time"

	"github.com/BeomSeogKim/crosshall/internal/ident"
)

func TestStartupRepoMarkerSurvivesMissingRendezvousAndDuplicates(t *testing.T) {
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
	if got := mappedEnrollmentRoot(t.Context(), rv, state, event{Identity: inside, CWD: repo}); got != repo {
		t.Fatalf("same canonical repository was refused: %q", got)
	}
	if got := mappedEnrollmentRoot(t.Context(), rv, state, event{Identity: outside, CWD: repo}); got != "" {
		t.Fatalf("session opened outside was allowed to recover in %q", got)
	}
	incomplete := ident.Identity{Vendor: "claude", Key: "incomplete"}
	if err := os.WriteFile(filepath.Join(state, startupRepoDir, incomplete.Digest()), []byte("1"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := mappedEnrollmentRoot(t.Context(), rv, state, event{Identity: incomplete, CWD: repo}); got != "" {
		t.Fatalf("session with incomplete marker was allowed to recover in %q", got)
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
}
