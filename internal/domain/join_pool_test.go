package domain

import (
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

func (f *fixture) joinPool(channel string) int {
	f.t.Helper()
	return f.count(`SELECT join_pool FROM channel WHERE name = ?`, channel)
}

func (f *fixture) rootTokens(s session) int {
	f.t.Helper()
	return f.count(`SELECT root_tokens FROM principal WHERE id = ?`, *f.caller(s).PrincipalID)
}

func pool(n int64) *int64 { return &n }

// A principal's first join to a channel takes one root token from the
// channel's pool, so a member can open its first exchange without an owner
// grant. Nothing else draws: not a rejoin, a retry, or a hook restore.
func TestFirstJoinDrawsOneRootTokenFromThePool(t *testing.T) {
	f := newFixture(t)
	created := decode[ChannelResult](t, f.owner(CreateChannel{Name: "checkout"}, "create"))
	if created.JoinPool != DefaultJoinPool || f.joinPool("checkout") != 8 {
		t.Fatalf("default pool: result %+v, stored %d", created, f.joinPool("checkout"))
	}
	writer := f.newSession("route")
	wantOK(t, f.join(writer, Join{Channel: "checkout", Alias: "writer"}, "join"))
	if f.rootTokens(writer) != 1 || f.joinPool("checkout") != 7 {
		t.Fatalf("first join: %d tokens, pool %d", f.rootTokens(writer), f.joinPool("checkout"))
	}
	wantOK(t, f.join(writer, Join{Channel: "checkout", Alias: "writer"}, "join"))
	wantOK(t, f.join(writer, Join{Channel: "checkout"}, "again"))
	wantOK(t, f.run(writer, Leave{}, "leave"))
	wantOK(t, f.join(writer, Join{Channel: "checkout", Alias: "writer"}, "back"))
	if f.rootTokens(writer) != 1 || f.joinPool("checkout") != 7 {
		t.Fatalf("a retry, repeat or rejoin drew again: %d tokens, pool %d", f.rootTokens(writer), f.joinPool("checkout"))
	}
	if n := f.count(`SELECT count(*) FROM budget_event WHERE reason = 'roots' AND delta = 1 AND principal_id = ?`, *f.caller(writer).PrincipalID); n != 1 {
		t.Fatalf("draw events: %d", n)
	}

	// The token opens an exchange without any grant.
	reviewer := f.newSession("route")
	wantOK(t, f.join(reviewer, Join{Channel: "checkout", Alias: "reviewer"}, ""))
	wantOK(t, f.ask(writer, Ask{To: []string{"reviewer"}, Body: "review?"}, "ask"))
}

// The pool bounds what fresh identities can draw: when it is empty, a first
// join gets nothing, and a pool of 0 makes every root token an owner grant.
func TestThePoolBoundsFirstJoins(t *testing.T) {
	f := newFixture(t)
	wantOK(t, f.owner(CreateChannel{Name: "small", Pool: pool(1)}, ""))
	wantOK(t, f.owner(CreateChannel{Name: "practice", Pool: pool(0)}, ""))
	a, b, c, d := f.newSession("route"), f.newSession("route"), f.newSession("route"), f.newSession("route")
	wantOK(t, f.join(a, Join{Channel: "small", Alias: "a"}, ""))
	wantOK(t, f.join(b, Join{Channel: "small", Alias: "b"}, ""))
	wantOK(t, f.join(c, Join{Channel: "practice", Alias: "c"}, ""))
	wantOK(t, f.join(d, Join{Channel: "practice", Alias: "d"}, ""))
	if f.rootTokens(a) != 1 || f.rootTokens(b) != 0 || f.rootTokens(c) != 0 || f.joinPool("small") != 0 || f.joinPool("practice") != 0 {
		t.Fatalf("tokens a=%d b=%d c=%d, pools small=%d practice=%d",
			f.rootTokens(a), f.rootTokens(b), f.rootTokens(c), f.joinPool("small"), f.joinPool("practice"))
	}
	wantCode(t, f.ask(c, Ask{To: []string{"d"}, Body: "x"}, ""), core.ExitRefused, core.CodeNoRootCredit)
	wantCode(t, f.owner(CreateChannel{Name: "negative", Pool: pool(-1)}, ""), core.ExitUsage, "")
}

// A first join through a repository default draws like a manual one.
func TestHookAutoJoinDrawsFromThePool(t *testing.T) {
	f, _ := adapterChannel(t)
	before := f.joinPool("checkout")
	s := f.newSession("claude")
	if got := f.hook(s, start("pid-1", "startup", f.now.UnixMilli())); got.Enrollment != "joined" {
		t.Fatalf("auto-join: %+v", got)
	}
	if f.rootTokens(s) != 1 || f.joinPool("checkout") != before-1 {
		t.Fatalf("auto-join: %d tokens, pool %d (was %d)", f.rootTokens(s), f.joinPool("checkout"), before)
	}
	f.hook(s, start("pid-2", "startup", f.now.UnixMilli()+1_000))
	if f.rootTokens(s) != 1 || f.joinPool("checkout") != before-1 {
		t.Fatalf("a restore drew again: %d tokens, pool %d", f.rootTokens(s), f.joinPool("checkout"))
	}
}

// Only the owner adds to a pool, and a grant names the pool alone.
func TestOwnerGrowsTheJoinPool(t *testing.T) {
	f := newFixture(t)
	wantOK(t, f.owner(CreateChannel{Name: "practice", Pool: pool(0)}, ""))
	owner := core.Caller{Role: core.RoleOwner}
	grant := BudgetGrant{Channel: "practice", Pool: 2}
	got := decode[BudgetGrantResult](t, f.ownerMutation(owner, grant, "pool"))
	if got != (BudgetGrantResult{Channel: "practice", Pool: 2, JoinPool: 2}) {
		t.Fatalf("pool grant %+v", got)
	}
	if replayed := decode[BudgetGrantResult](t, f.ownerMutation(owner, grant, "pool")); replayed != got || f.joinPool("practice") != 2 {
		t.Fatalf("replayed pool grant %+v, pool %d", replayed, f.joinPool("practice"))
	}
	m := f.newSession("route")
	wantOK(t, f.join(m, Join{Channel: "practice", Alias: "m"}, ""))
	wantCode(t, f.ownerMutation(f.caller(m), BudgetGrant{Channel: "practice", Pool: 1}, ""), core.ExitRefused, core.CodeOwnerOnly)
	wantCode(t, f.ownerMutation(owner, BudgetGrant{Channel: "practice", Alias: "m", Roots: 1, Pool: 1}, ""), core.ExitUsage, "")
	if f.rootTokens(m) != 1 || f.joinPool("practice") != 1 {
		t.Fatalf("after grant and join: %d tokens, pool %d", f.rootTokens(m), f.joinPool("practice"))
	}
}
