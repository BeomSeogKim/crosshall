//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/domain"
)

func TestRestoreDepartedBindingAfterAliasReuse(t *testing.T) {
	w := newWorld(t)
	owner, original, successor, fresh, editor := w.owner(), w.member(), w.member(), w.member(), w.member()
	// Grant-only, so the credits the test follows are the owner's grants.
	owner.must("owner", "channel", "create", "checkout", "--pool", "0")
	editor.must("join", "checkout", "--as", "editor")
	originalJoin := []string{"join", "checkout", "--as", "reviewer", "--key", "original-join"}
	original.must(originalJoin...)
	departed := restoreBinding(t, owner, "live")
	grantRoots(t, w, "reviewer", 1)
	grantRoots(t, w, "editor", 1)
	first := num(t, editor.must("ask", "@reviewer", "Check the draft."), "id")
	original.must("leave")

	successor.must("join", "checkout", "--as", "reviewer")
	reused := restoreBinding(t, owner, "live")
	departed.State = "departed"
	if got := restoreBinding(t, owner, "departed"); got != departed || reused.ID == got.ID || reused.PrincipalID == got.PrincipalID {
		t.Fatal("owner status must distinguish the departed and live holders of the same alias")
	}
	second := num(t, editor.must("ask", "@reviewer", "Review the final copy."), "id")
	keyFor := func(id int64, candidate *participant, operation string) []string {
		return []string{"owner", "restore", strconv.FormatInt(id, 10), "--new-key", "route:" + candidate.route, "--key", operation}
	}
	refused := func(o outcome, exit int, code string) {
		t.Helper()
		if o.exit != exit || o.obj["code"] != code {
			t.Fatalf("expected exit %d and %s, got %d and %v", exit, code, o.exit, o.obj["code"])
		}
	}
	refused(owner.run(keyFor(departed.ID, successor, "already-joined")...), 3, "key_in_use")
	restore := keyFor(departed.ID, fresh, "replace-original")
	committed := owner.must(restore...)
	if replay := owner.must(restore...); replay.stdout != committed.stdout || strings.Contains(replay.stdout, fresh.route) {
		t.Fatal("restore replay must return the same result without exposing the replacement key")
	}
	refused(owner.run(keyFor(departed.ID, w.member(), "replace-original")...), 3, "key_conflict")
	refused(owner.run(keyFor(reused.ID, fresh, "replace-original")...), 3, "key_conflict")
	refused(owner.run(keyFor(departed.ID, original, "retired-candidate")...), 3, "key_in_use")

	// First use the original member directory, then force first contact by
	// removing its disposable generation file. Even a pre-restore replay fails.
	refused(original.run(originalJoin...), 4, "key_retired")
	if err := os.Remove(filepath.Join(original.leaf(), "gen")); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	refused(original.run(originalJoin...), 4, "key_retired")
	refused(fresh.run("inbox"), 4, "not_enrolled")
	refused(fresh.run("join", "checkout"), 3, "alias_taken")
	if got := restoreBinding(t, owner, "live"); got != reused {
		t.Fatal("restoring a departed binding changed its live alias successor")
	}

	firstID, secondID := strconv.FormatInt(first, 10), strconv.FormatInt(second, 10)
	if got := ids(successor.must("read").obj["messages"]); !slices.Equal(got, []int64{second}) {
		t.Fatalf("alias successor read %v, want only its own request %d", got, second)
	}
	refused(successor.run("reply", firstID, "wrong recipient"), 3, "not_obligated")
	successor.must("reply", secondID, "Final copy approved.")
	refused(successor.run("ask", "@editor", "Open another review.", "--new"), 3, "no_root_credit")
	grantRoots(t, w, "reviewer", 2)
	successor.must("leave")

	fresh.must("join", "checkout")
	departed.State = "live"
	if got := restoreBinding(t, owner, "live"); got != departed {
		t.Fatalf("replacement rejoined as binding %d principal %d, want binding %d principal %d",
			got.ID, got.PrincipalID, departed.ID, departed.PrincipalID)
	}
	if got := ids(fresh.must("read").obj["messages"]); !slices.Equal(got, []int64{first}) {
		t.Fatalf("replacement read %v, want original request %d", got, first)
	}
	refused(fresh.run("reply", secondID, "wrong recipient"), 3, "not_obligated")
	fresh.must("reply", firstID, "The draft is ready.")
	fresh.must("ask", "@editor", "Open the original member's last review.", "--new")
	refused(fresh.run("ask", "@editor", "Open a further review.", "--new"), 3, "no_root_credit")
}

func restoreBinding(t *testing.T, owner *participant, state string) domain.StatusBinding {
	t.Helper()
	var status domain.StatusResult
	o := owner.must("owner", "status", "--channel", "checkout")
	if err := json.Unmarshal([]byte(o.stdout), &status); err != nil {
		t.Fatal(err)
	}
	for _, channel := range status.Channels {
		for _, b := range channel.Bindings {
			if b.Alias == "reviewer" && b.State == state && b.ID > 0 && b.PrincipalID > 0 {
				return b
			}
		}
	}
	t.Fatalf("owner status missing the %s reviewer binding: %s", state, o.stdout)
	return domain.StatusBinding{}
}
