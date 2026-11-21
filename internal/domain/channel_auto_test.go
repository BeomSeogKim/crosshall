package domain

import (
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func TestRepoChannelNameIsAValidChannelName(t *testing.T) {
	for root, want := range map[string]string{
		"/work/crosshall": "crosshall",
		"/work/Shop API":  "shop-api",
		"/work/store.git": "store",
		"/work/__init__":  "init",
		"/work/한글":        "repo",
		"/work/a--b..c":   "a-b-c",
		"/work/x-0123456789-0123456789-0123456789-0123456789": "x-0123456789-0123456789-01234567",
	} {
		got := repoChannelName(root)
		if got != want || !ValidName(got) {
			t.Errorf("repoChannelName(%q) = %q, want %q (valid %v)", root, got, want, ValidName(got))
		}
	}
}

// The dispatcher takes the worktree's default, else the repository's, else
// the repository's own channel, inside the enrolment's transaction: a
// default the owner committed after the hook read its snapshot still wins.
func TestHookEnrolmentResolvesItsChannelInTheTransaction(t *testing.T) {
	f := newFixture(t)
	f.policy("claude", true, 1)
	wantOK(t, f.owner(ChannelAuto{On: true}, "auto"))
	wantOK(t, f.owner(CreateChannel{Name: "side"}, "c"))
	const repository, side = "/work/shop", "/work/shop-side"
	enrolIn := func(worktree, repository string, create bool) wire.HookResult {
		t.Helper()
		cmd := start("pid-1", "startup", f.now.UnixMilli())
		cmd.RepoRoot, cmd.Repository, cmd.CreateChannel = worktree, repository, create
		return f.hook(f.newSession("claude"), cmd)
	}
	enrol := func(worktree string, create bool) wire.HookResult { return enrolIn(worktree, repository, create) }
	channels := func() int { return f.count(`SELECT COUNT(*) FROM channel`) }

	// The hook did not see a real work tree: the repository gets no channel.
	if got := enrol(side, false); got.Enrollment != "none" || got.Hint != "" || channels() != 1 {
		t.Fatalf("a start outside a work tree: %+v, %d channels", got, channels())
	}
	// The hook's snapshot had no default for the worktree; the owner has
	// committed one since.
	wantOK(t, f.owner(ChannelDefault{Channel: "side", RepoRoot: side}, "d"))
	if got := enrol(side, true); got.Enrollment != "joined" || got.Channel != "side" || got.ChannelCreated || channels() != 1 {
		t.Fatalf("a worktree with the owner's default: %+v, %d channels", got, channels())
	}
	if got := enrol(repository, true); got.Enrollment != "joined" || got.Channel != "shop" || !got.ChannelCreated {
		t.Fatalf("the repository's main checkout: %+v", got)
	}
	if got := enrol("/work/shop-other", true); got.Enrollment != "joined" || got.Channel != "shop" || got.ChannelCreated || channels() != 2 {
		t.Fatalf("another worktree of the repository: %+v, %d channels", got, channels())
	}
	// The created channel's default is automatic until the owner names it.
	automatic := func() int { return f.count(`SELECT automatic FROM repo_default WHERE repo_root = ?`, repository) }
	if automatic() != 1 {
		t.Fatal("a created channel's default is not marked automatic")
	}
	wantOK(t, f.owner(ChannelDefault{Channel: "shop", RepoRoot: repository}, "own"))
	if automatic() != 0 {
		t.Fatal("the owner's command left the default automatic")
	}
	// With the switch off the repository's channel stays its default.
	wantOK(t, f.owner(ChannelAuto{On: false}, "off"))
	if got := enrol("/work/shop-third", false); got.Enrollment != "joined" || got.Channel != "shop" {
		t.Fatalf("a worktree after the switch went off: %+v", got)
	}
	if got := enrolIn("/work/elsewhere", "/work/elsewhere", true); got.Enrollment != "none" || channels() != 2 {
		t.Fatalf("an unknown repository with the switch off: %+v, %d channels", got, channels())
	}
}
