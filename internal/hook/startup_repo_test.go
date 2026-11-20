package hook

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
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
	if err := os.WriteFile(filepath.Join(state, StatePolicyName), []byte("claude 1 0 1\n"), 0600); err != nil {
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
	paths := Paths{State: state, Rendezvous: filepath.Join(state, "rendezvous")}
	fire := func(id ident.Identity, source, cwd string) {
		t.Helper()
		t.Setenv("CLAUDE_CODE_SESSION_ID", id.Key)
		body := fmt.Sprintf(`{"hook_event_name":"SessionStart","session_id":%q,"source":%q,"cwd":%q}`, id.Key, source, cwd)
		Run(time.Now(), []string{"claude", "SessionStart"}, strings.NewReader(body), io.Discard, func() (Paths, error) {
			return paths, nil
		})
	}
	outside := ident.Identity{Vendor: "claude", Key: "opened-outside"}
	fire(outside, "resume", repo)
	if got := startupRepoDigest(state, outside); got != "" {
		t.Fatalf("resume established startup evidence %q", got)
	}
	fire(outside, "startup", state)
	fire(outside, "startup", repo)
	if got := startupRepoDigest(state, outside); got != "-\n" {
		t.Fatalf("duplicate startup replaced the outside marker: %q", got)
	}
	inside := ident.Identity{Vendor: "claude", Key: "opened-in-project"}
	fire(inside, "startup", nested)
	if got := startupRepoDigest(state, inside); got != repoDigestRecord(repo) || strings.Contains(got, repo) {
		t.Fatalf("marker did not contain only the canonical repo digest: %q", got)
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
}
