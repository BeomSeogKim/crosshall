package domain

import (
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

func (f *fixture) setRepoDefault(caller core.Caller, cmd ChannelDefault, key string) core.Result {
	return f.exec(caller, Request{Command: cmd, OpKey: key})
}

func (f *fixture) findRepoDefault(root string) (channelRow, bool) {
	f.t.Helper()
	tx, err := f.db.Begin()
	if err != nil {
		f.t.Fatal(err)
	}
	defer tx.Rollback()
	c, found, err := defaultChannel(f.t.Context(), tx, root)
	if err != nil {
		f.t.Fatal(err)
	}
	return c, found
}

func TestRepositoryDefaultRequiresOwnerAndExistingChannel(t *testing.T) {
	f := newFixture(t)
	owner := core.Caller{Role: core.RoleOwner}
	member := core.Caller{Role: core.RoleMember}
	repo := t.TempDir()
	active := ChannelDefault{Channel: "checkout", RepoRoot: repo}
	if _, found := f.findRepoDefault(repo); found {
		t.Fatal("an opted-out repository already has a channel")
	}
	wantCode(t, f.setRepoDefault(owner, active, "unknown"), core.ExitRefused, core.CodeNotFound)
	wantOK(t, f.owner(CreateChannel{Name: active.Channel}, "create"))
	wantCode(t, f.setRepoDefault(member, active, "member"), core.ExitRefused, core.CodeOwnerOnly)
	wantCode(t, f.setRepoDefault(owner, active, ""), core.ExitUsage, "")
	if _, found := f.findRepoDefault(repo); found {
		t.Fatal("a refused request opted in the repository")
	}
	wantOK(t, f.setRepoDefault(owner, active, "opt-in"))
	if foundChannel, found := f.findRepoDefault(repo); !found || foundChannel.name != active.Channel {
		t.Fatalf("lookup returned %+v, found=%t", foundChannel, found)
	}
	if _, found := f.findRepoDefault(t.TempDir()); found {
		t.Fatal("a default for another repository leaked")
	}
}

func TestRepositoryDefaultReplayPreservesANewerMapping(t *testing.T) {
	f := newFixture(t)
	owner := core.Caller{Role: core.RoleOwner}
	wantOK(t, f.owner(CreateChannel{Name: "checkout"}, "create-one"))
	wantOK(t, f.owner(CreateChannel{Name: "reports"}, "create-two"))
	repo := t.TempDir()
	first := ChannelDefault{Channel: "checkout", RepoRoot: repo}
	next := ChannelDefault{Channel: "reports", RepoRoot: repo}
	wantOK(t, f.setRepoDefault(owner, first, "first"))
	wantOK(t, f.setRepoDefault(owner, next, "next"))
	replayed := decode[ChannelDefaultResult](t, f.setRepoDefault(owner, first, "first"))
	if replayed.Channel != first.Channel || replayed.RepoRoot != repo {
		t.Fatalf("unexpected replay: %+v", replayed)
	}
	wantCode(t, f.setRepoDefault(owner, next, "first"), core.ExitRefused, core.CodeKeyConflict)
	if channel, found := f.findRepoDefault(repo); !found || channel.name != next.Channel {
		t.Fatalf("replay changed mapping to %+v, found=%t", channel, found)
	}
	if changes := f.count("SELECT count(*) FROM event WHERE kind = 'channel_default_set'"); changes != 2 {
		t.Fatalf("got %d default changes, want 2", changes)
	}
}
