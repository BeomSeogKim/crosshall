//go:build e2e

package e2e

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOwnerPublishesAdapterPolicyAndDefaults(t *testing.T) {
	w := newWorld(t)
	owner := w.owner()
	owner.must("owner", "channel", "create", "checkout")
	policy := filepath.Join(w.rendezvous(), "policy")
	defaults := filepath.Join(w.rendezvous(), "repo-defaults")
	assertSnapshot := func(path, want string) {
		t.Helper()
		b, err := os.ReadFile(path)
		if err != nil || string(b) != want {
			t.Fatalf("snapshot %s: %q, want %q (%v)", filepath.Base(path), b, want, err)
		}
	}
	assertSnapshot(policy, "agy 0 0 0\nclaude 0 0 0\ncodex 0 0 0\n")
	assertSnapshot(defaults, "")
	owner.must("--key", "enable", "owner", "adapter", "set", "codex", "--enabled", "on")
	assertSnapshot(policy, "agy 0 0 0\nclaude 0 0 0\ncodex 1 0 1\n")
	owner.must("owner", "adapter", "set", "codex", "--enabled", "off")
	owner.must("--key", "enable", "owner", "adapter", "set", "codex", "--enabled", "on")
	// The reply can be an old immutable result. The mirror must stay current.
	assertSnapshot(policy, "agy 0 0 0\nclaude 0 0 0\ncodex 0 0 2\n")

	repo := filepath.Join(w.home, "checkout")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0600); err != nil {
		t.Fatal(err)
	}
	owner.must("owner", "channel", "default", "checkout", "--repo", repo)
	wantHash := fmt.Sprintf("%x\n", sha256.Sum256([]byte(repo)))
	assertSnapshot(defaults, wantHash)
	purged := w.rendezvous() + ".purged"
	if err := os.Rename(w.rendezvous(), purged); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(purged) })
	deadline := time.Now().Add(fixtureEventTimeout)
	for {
		raw, err := os.ReadFile(filepath.Join(w.rendezvous(), "ready"))
		if err == nil && strings.Contains(string(raw), "instance") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("dispatcher did not restore its rendezvous, last ready marker %q: %v", raw, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	assertSnapshot(policy, "agy 0 0 0\nclaude 0 0 0\ncodex 0 0 2\n")
	assertSnapshot(defaults, wantHash)
}
