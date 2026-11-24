package domain

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// takeoverJoin runs join --as alias --takeover in #checkout as the
// dispatcher would, with the caller's session key.
func (f *fixture) takeoverJoin(s session, alias, key string) core.Result {
	f.t.Helper()
	c := f.caller(s)
	req := Request{OpKey: key, Command: Join{Channel: "checkout", Alias: alias, Takeover: true}, Vendor: s.vendor, SessionKey: s.key}
	if c.Role == core.RoleUnenrolled {
		req.Enroll = enroll(s)
	}
	return f.exec(c, req)
}

func (f *fixture) takeoverRequest(s session, alias, key string) TakeoverRequest {
	f.t.Helper()
	r := f.takeoverJoin(s, alias, key)
	wantOK(f.t, r)
	got := decode[JoinResult](f.t, r)
	if got.Takeover == nil {
		f.t.Fatalf("join --takeover joined instead of asking: %+v", got)
	}
	return *got.Takeover
}

// endedCodexMember is a Codex member of #checkout whose session ended.
func endedCodexMember(t *testing.T, f *fixture, alias string) (session, int64) {
	t.Helper()
	s, binding := codexMember(t, f, alias)
	f.hook(s, codexEnded(publish.ModeDaemon, f.now.UnixMilli()+1))
	f.now = f.now.Add(time.Minute)
	return s, binding
}

func takeoverFixture(t *testing.T) *fixture {
	t.Helper()
	f, _ := adapterChannel(t)
	f.policy("codex", true, 1)
	f.svc.RestoreKey = ident.RestoreKey
	return f
}

// A session that never joined asks for an ended member's alias, the owner
// grants it, and the session's second join --takeover makes it that
// member: the binding, its owed reply and its history stay.
func TestNeverEnrolledSessionTakesOverAnEndedMember(t *testing.T) {
	f := takeoverFixture(t)
	writer := session{vendor: "codex"}
	if err := f.db.QueryRow(`SELECT k.vendor, k.key FROM principal_key k JOIN binding b ON b.principal_id = k.principal_id
		WHERE b.alias = 'writer'`).Scan(&writer.vendor, &writer.key); err != nil {
		t.Fatal(err)
	}
	old, binding := codexMember(t, f, "vega")
	oldPrincipal := f.principalOf(old)
	owed := f.turnEndAsk(writer, "vega", "owed")
	f.hook(old, codexEnded(publish.ModeDaemon, f.now.UnixMilli()+1))
	f.now = f.now.Add(time.Minute)

	fresh := f.newSession("codex")
	refused := f.join(fresh, Join{Channel: "checkout", Alias: "vega"}, "plain")
	wantCode(t, refused, core.ExitRefused, core.CodeAliasTaken)
	if hint := decode[Refusal](t, refused).Hint; !strings.Contains(hint, "crosshall join checkout --as vega --takeover") {
		t.Fatalf("refusal does not offer the takeover: %q", hint)
	}

	req := f.takeoverRequest(fresh, "vega", "ask-1")
	if req.AtGrant || req.Session != fresh.digest()[:12] {
		t.Fatalf("request %+v", req)
	}
	if again := f.takeoverRequest(fresh, "vega", "ask-2"); again.ID != req.ID {
		t.Fatalf("a repeated request got ID %d, want %d", again.ID, req.ID)
	}
	if f.count(`SELECT COUNT(*) FROM event WHERE kind = 'takeover_requested'`) != 1 ||
		f.count(`SELECT COUNT(*) FROM event WHERE instr(detail, ?) > 0`, fresh.key) != 0 ||
		f.count(`SELECT COUNT(*) FROM principal_key WHERE key = ?`, fresh.key) != 0 {
		t.Fatal("the request enrolled the session, wrote its key, or repeated its event")
	}

	lines := decode[OwnerWhoResult](t, f.owner(OwnerWho{}, "")).Takeovers
	if len(lines) != 1 || lines[0].ID != req.ID || lines[0].BindingID != binding || lines[0].Alias != "vega" ||
		lines[0].Verified || len(lines[0].Owes) != 1 || lines[0].Owes[0] != owed {
		t.Fatalf("owner who takeovers %+v", lines)
	}

	grant := decode[OwnerTakeoverResult](t, f.owner(OwnerTakeover{ID: req.ID}, "grant"))
	if grant.Completed || grant.ExpiresMS != f.now.Add(takeoverGrantLife).UnixMilli() {
		t.Fatalf("grant %+v", grant)
	}
	if f.caller(fresh).Role != core.RoleUnenrolled {
		t.Fatal("the grant enrolled the session before it asked again")
	}
	wantCode(t, f.owner(OwnerTakeover{ID: req.ID}, "grant-again"), core.ExitRefused, core.CodeBindingChanged)

	other := f.newSession("codex")
	wantCode(t, f.takeoverJoin(other, "vega", "other-complete"), core.ExitOK, "")
	if f.caller(other).Role != core.RoleUnenrolled {
		t.Fatal("a session whose key differs completed the grant")
	}
	// That other session's request replaced the granted one.
	wantCode(t, f.takeoverJoin(fresh, "vega", "complete"), core.ExitOK, "")
	if f.caller(fresh).Role != core.RoleUnenrolled {
		t.Fatal("a replaced grant completed")
	}

	f.now = f.now.Add(time.Minute)
	req = f.takeoverRequest(fresh, "vega", "ask-3")
	wantOK(t, f.owner(OwnerTakeover{ID: req.ID}, "grant-3"))
	joined := decode[JoinResult](t, f.takeoverJoin(fresh, "vega", "complete-3"))
	if joined.Takeover != nil || joined.Alias != "vega" || !joined.Restored {
		t.Fatalf("completion %+v", joined)
	}
	if f.principalOf(fresh) != oldPrincipal || f.keyState(old) != "retired" ||
		f.count(`SELECT COUNT(*) FROM principal_key WHERE principal_id = ? AND state = 'live'`, oldPrincipal) != 1 ||
		f.count(`SELECT COUNT(*) FROM adapter_launch WHERE principal_id = ? AND fenced = 0 AND ended_hook_at_ms IS NULL`, oldPrincipal) != 0 ||
		f.count(`SELECT COUNT(*) FROM event WHERE kind = 'binding_restored'`) != 1 {
		t.Fatal("completion did not restore the binding to the session")
	}
	if f.count(`SELECT COUNT(*) FROM obligation WHERE message_id = ? AND principal_id = ? AND state = 'open'`, owed, oldPrincipal) != 1 {
		t.Fatal("the owed reply did not stay with the binding")
	}
	if len(decode[OwnerWhoResult](t, f.owner(OwnerWho{}, "")).Takeovers) != 0 {
		t.Fatal("a completed takeover is still listed")
	}
	given := inTx(t, f, func(ctx context.Context, tx store.Tx) (string, error) {
		return TakeoverGivenAway(ctx, tx, old.digest())
	})
	if !strings.Contains(given, "@vega was given to another session (takeover ") {
		t.Fatalf("old conversation's message %q", given)
	}
}

// A grant waits an hour for a session that never joined.
func TestTakeoverGrantExpires(t *testing.T) {
	f := takeoverFixture(t)
	endedCodexMember(t, f, "vega")
	fresh := f.newSession("codex")
	req := f.takeoverRequest(fresh, "vega", "ask")
	wantOK(t, f.owner(OwnerTakeover{ID: req.ID}, "grant"))
	f.now = f.now.Add(takeoverGrantLife + time.Second)
	if len(decode[OwnerWhoResult](t, f.owner(OwnerWho{}, "")).Takeovers) != 0 {
		t.Fatal("an expired grant is still listed")
	}
	wantCode(t, f.owner(OwnerTakeover{ID: req.ID}, "grant-late"), core.ExitRefused, core.CodeBindingChanged)
	// The session asks again: a new request, which the owner can grant.
	again := f.takeoverRequest(fresh, "vega", "late")
	if again.ID == req.ID || f.caller(fresh).Role != core.RoleUnenrolled {
		t.Fatalf("asking after an expired grant gave %+v", again)
	}
	wantOK(t, f.owner(OwnerTakeover{ID: again.ID}, "grant-again"))
	wantOK(t, f.takeoverJoin(fresh, "vega", "complete"))
	if f.caller(fresh).Role != core.RoleMember {
		t.Fatal("the second grant did not complete")
	}
}

// An enrolled session (a hook joined it to another channel) asks; the
// owner's grant moves its key and leaf to the member at once, retires the
// member's old keys and closes the session's own membership.
func TestEnrolledSessionTakesOverAtGrant(t *testing.T) {
	useCodexSender(t)
	f := takeoverFixture(t)
	codexEvidence(t, f, "0.171.0")
	wantOK(t, f.owner(CreateChannel{Name: "other"}, "other"))
	old, binding := endedCodexMember(t, f, "vega")
	oldPrincipal := f.principalOf(old)
	asker := f.newSession("codex")
	wantOK(t, f.join(asker, Join{Channel: "other", Alias: "codex3"}, "asker-join"))
	f.hook(asker, codexStart("startup", publish.ModeDaemon, f.now.UnixMilli()+1))
	askerPrincipal := f.principalOf(asker)
	askerBinding := bindingIDOf(t, f, askerPrincipal)

	refused := f.join(asker, Join{Channel: "checkout", Alias: "vega", Move: true}, "plain")
	if !strings.Contains(decode[Refusal](t, refused).Hint, "--takeover") {
		t.Fatalf("an enrolled session's refusal does not offer the takeover: %+v", decode[Refusal](t, refused))
	}
	req := f.takeoverRequest(asker, "vega", "ask")
	if !req.AtGrant {
		t.Fatalf("request %+v", req)
	}
	grant := decode[OwnerTakeoverResult](t, f.owner(OwnerTakeover{ID: req.ID}, "grant"))
	if !grant.Completed || grant.BindingID != binding {
		t.Fatalf("grant %+v", grant)
	}
	if f.principalOf(asker) != oldPrincipal || f.keyState(old) != "retired" ||
		f.count(`SELECT COUNT(*) FROM principal_key WHERE principal_id = ? AND state = 'live'`, oldPrincipal) != 1 ||
		f.count(`SELECT COUNT(*) FROM principal_key WHERE principal_id = ?`, askerPrincipal) != 0 ||
		f.count(`SELECT principal_id FROM leaf WHERE digest = ?`, asker.digest()) != int(oldPrincipal) ||
		f.count(`SELECT COUNT(*) FROM leaf WHERE digest = ? AND state = 'fenced'`, old.digest()) != 1 {
		t.Fatal("the grant did not move the session's key and leaf to the member")
	}
	if f.count(`SELECT COUNT(*) FROM binding WHERE id = ? AND state = 'departed'`, askerBinding) != 1 ||
		f.count(`SELECT COUNT(*) FROM binding WHERE id = ? AND state = 'live'`, binding) != 1 {
		t.Fatal("the session's own membership did not close, or the member's did")
	}
	wantCode(t, f.owner(OwnerTakeover{ID: req.ID}, "grant-again"), core.ExitRefused, core.CodeBindingChanged)

	// The running session gets no new start: its launch and route came with
	// it, so its next prompt runs under the member, and a notice reaches it.
	if f.count(`SELECT COUNT(*) FROM adapter_launch WHERE principal_id = ? AND ended_hook_at_ms IS NULL`, askerPrincipal) != 0 {
		t.Fatal("the session's launch stayed open under its old principal")
	}
	f.hook(asker, codexPrompt("t1", f.now.UnixMilli()+1))
	if _, _, _, _, state, reason, _ := routeState(t, f, binding); state != "active" ||
		f.count(`SELECT COUNT(*) FROM adapter_launch WHERE principal_id = ? AND ended_hook_at_ms IS NULL`, oldPrincipal) != 1 {
		t.Fatalf("after the move the member's route is %s (%s), or its launch is not bound under the member", state, reason)
	}
	proveCodexRoutes(t, f)
	if got := publishStateOf(t, f, binding); got.State != "available" {
		t.Fatalf("after the move the member's proven route reads %s (%s)", got.State, got.Reason)
	}
	writer := session{}
	if err := f.db.QueryRow(`SELECT k.vendor, k.key FROM principal_key k JOIN binding b ON b.principal_id = k.principal_id
		WHERE b.alias = 'writer'`).Scan(&writer.vendor, &writer.key); err != nil {
		t.Fatal(err)
	}
	f.turnEndAsk(writer, "vega", "after-move")
	end := stop(SessionLaunch, "t1", f.now.UnixMilli()+1)
	end.Profile = codexProfile(publish.ModeDaemon)
	f.hook(asker, end)
	if !codexReserve(t, f, binding) {
		t.Fatal("a notice cannot reach the member after the move")
	}
}

// An enrolled session that owes or awaits a reply cannot ask: its own
// membership would close with the request open.
func TestTakeoverRefusesAnAskerWithOpenRequests(t *testing.T) {
	f := takeoverFixture(t)
	endedCodexMember(t, f, "vega")
	asker, _ := codexMember(t, f, "asker")
	writer := session{}
	if err := f.db.QueryRow(`SELECT k.vendor, k.key FROM principal_key k JOIN binding b ON b.principal_id = k.principal_id
		WHERE b.alias = 'writer'`).Scan(&writer.vendor, &writer.key); err != nil {
		t.Fatal(err)
	}
	f.turnEndAsk(writer, "asker", "owed")
	wantCode(t, f.takeoverJoin(asker, "vega", "ask"), core.ExitRefused, core.CodeBindingChanged)
	if f.count(`SELECT COUNT(*) FROM event WHERE kind = 'takeover_requested'`) != 0 {
		t.Fatal("a refused request was recorded")
	}
}

// join --takeover asks only for a live member of the same agent whose
// session ended, and never for the owner alias.
func TestTakeoverRefusals(t *testing.T) {
	f := takeoverFixture(t)
	codexMember(t, f, "live")
	endedCodexMember(t, f, "vega")
	_, departed := endedCodexMember(t, f, "gone")
	if _, err := f.db.Exec(`UPDATE binding SET state = 'departed' WHERE id = ?`, departed); err != nil {
		t.Fatal(err)
	}
	fresh := f.newSession("codex")
	claude := f.newSession("claude")
	cases := []struct {
		s     session
		alias string
		code  core.Code
	}{
		{fresh, "live", core.CodeAliasTaken},
		{claude, "vega", core.CodeAliasTaken},
		{fresh, "gone", core.CodeNotFound},
		{fresh, "owner", core.CodeAliasTaken},
	}
	for _, c := range cases {
		r := f.takeoverJoin(c.s, c.alias, "")
		if r.Status == core.ExitOK || r.Code != c.code {
			t.Errorf("join --as %s --takeover by %s: %v %s, want %s", c.alias, c.s.vendor, r.Status, r.Code, c.code)
		}
	}
	if hint := decode[Refusal](t, f.join(fresh, Join{Channel: "checkout", Alias: "live"}, "")).Hint; strings.Contains(hint, "--takeover") {
		t.Fatalf("a live member's alias offers a takeover: %q", hint)
	}
	if hint := decode[Refusal](t, f.join(claude, Join{Channel: "checkout", Alias: "vega"}, "")).Hint; strings.Contains(hint, "--takeover") {
		t.Fatalf("another agent's member offers a takeover: %q", hint)
	}
	if f.count(`SELECT COUNT(*) FROM event WHERE kind = 'takeover_requested'`) != 0 || f.count(`SELECT COUNT(*) FROM principal_key WHERE key IN (?, ?)`, fresh.key, claude.key) != 0 {
		t.Fatal("a refused request was recorded or enrolled the session")
	}
	wantCode(t, f.owner(OwnerTakeover{ID: 999}, "missing"), core.ExitRefused, core.CodeNotFound)
}

// A member whose session is seen again before the grant cannot be taken.
func TestTakeoverGrantRefusedWhenTheMemberIsBack(t *testing.T) {
	f := takeoverFixture(t)
	old, _ := endedCodexMember(t, f, "vega")
	req := f.takeoverRequest(f.newSession("codex"), "vega", "ask")
	wantOK(t, f.exec(f.caller(old), Request{Command: Status{}}))
	wantCode(t, f.owner(OwnerTakeover{ID: req.ID}, "grant"), core.ExitRefused, core.CodeBindingChanged)
}

// eventGuard fails the test when a statement reads the event log.
type eventGuard struct {
	store.Tx
	t *testing.T
}

func (g eventGuard) check(query string) {
	if strings.Contains(strings.Join(strings.Fields(query), " "), "FROM event ") {
		g.t.Errorf("a hook read the event log: %s", query)
	}
}

func (g eventGuard) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	g.check(q)
	return g.Tx.QueryContext(ctx, q, args...)
}

func (g eventGuard) QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row {
	g.check(q)
	return g.Tx.QueryRowContext(ctx, q, args...)
}

// No hook reads the event log for a takeover, nor does the refused join
// that offers one: only owner commands and join --takeover scan it.
func TestTakeoverKeepsHooksOffTheEventLog(t *testing.T) {
	f := takeoverFixture(t)
	endedCodexMember(t, f, "vega")
	asker := f.newSession("codex")
	wantOK(t, f.join(asker, Join{Channel: "checkout", Alias: "asker"}, "asker-join"))
	req := f.takeoverRequest(asker, "vega", "ask")
	wantOK(t, f.owner(OwnerTakeover{ID: req.ID}, "grant"))

	guarded := func(s session, req Request) core.Result {
		caller := f.caller(s) // before the transaction takes the one connection
		tx, err := f.db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		r, err := f.svc.Handle(context.Background(), eventGuard{tx, t}, caller, req)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	at := f.now.UnixMilli() + 1
	for _, cmd := range []Command{codexStart("resume", publish.ModeDaemon, at), codexPrompt("t1", at), codexEnded(publish.ModeDaemon, at)} {
		guarded(asker, Request{Command: cmd, Vendor: "codex", OpKey: hookKey(cmd), Adapter: &AdapterContext{Generation: "g1"}})
	}
	fresh := f.newSession("codex")
	guarded(fresh, Request{Command: Join{Channel: "checkout", Alias: "asker"}, Vendor: "codex", SessionKey: fresh.key, Enroll: enroll(fresh)})
	guarded(fresh, Request{Command: Who{}})
}
