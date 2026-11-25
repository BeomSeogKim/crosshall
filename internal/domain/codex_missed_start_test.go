package domain

import (
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// #252, Codex: a session whose start the dispatcher missed gets, from a
// prompt that names its server, the route and mode that start would have
// given. Nothing else registers a route from a prompt.

// codexServerPrompt is a Codex prompt in mode, naming the managed server when
// server is set; recovering makes it the event that may enroll a session.
func codexServerPrompt(mode string, server *wire.Host, turn string, at int64, recovering bool) HookEvent {
	cmd := prompt(SessionLaunch, turn, at)
	if recovering {
		cmd = recoveryPrompt(SessionLaunch, turn, at)
	}
	cmd.Profile, cmd.LaunchVerified = codexProfile(mode), false
	if server != nil {
		cmd.Host, cmd.VendorHome = server, codexHome
	}
	return cmd
}

func codexFixture(t *testing.T) (*fixture, session) {
	t.Helper()
	f, _ := adapterChannel(t)
	f.policy("codex", true, 1)
	return f, f.newSession("codex")
}

type codexRoute struct {
	gen          int64
	mode, state  string
	hostPID      int64
	home, thread string
}

func (f *fixture) codexRoute(s session) (codexRoute, bool) {
	f.t.Helper()
	var r codexRoute
	err := f.db.QueryRow(`SELECT r.generation, r.mode, r.state, COALESCE(r.host_pid, 0), COALESCE(r.vendor_home, ''), COALESCE(r.thread_id, '')
		FROM publish_route r JOIN binding b ON b.id = r.binding_id WHERE b.principal_id = ?`, f.principalOf(s)).
		Scan(&r.gen, &r.mode, &r.state, &r.hostPID, &r.home, &r.thread)
	return r, err == nil
}

func TestARecoveringCodexPromptRegistersTheRouteItsStartWould(t *testing.T) {
	f, s := codexFixture(t)
	at := f.now.UnixMilli()
	server := codexServer
	if r := f.hookAs(s, codexServerPrompt(publish.ModeDaemon, &server, "t1", at+10, true)); r.Enrollment != "joined" {
		t.Fatalf("recovering prompt: %+v, want joined", r)
	}
	if got := f.launches(s); len(got) != 1 || got[0] != (launchRow{SessionLaunch, false, 0, publish.ModeDaemon}) {
		t.Fatalf("launches %+v, want the session launch, start unknown, in the prompt's mode", got)
	}
	route, found := f.codexRoute(s)
	if !found || route.mode != publish.ModeDaemon || route.state != "active" || route.hostPID != codexServer.PID ||
		route.home != codexHome || route.thread != s.key {
		t.Fatalf("route %+v (found %t), want the daemon route the start would register", route, found)
	}
	if f.lateBinds() != 0 {
		t.Fatal("a Codex session launch was bound late")
	}
}

// Without a named server a prompt registers no route, which no sender could
// reach and no later prompt would replace, and keeps today's unverified mode.
func TestARecoveringCodexPromptWithoutItsServerKeepsTodaysRows(t *testing.T) {
	for _, mode := range []string{publish.ModeDaemon, publish.ModeInProcess} {
		t.Run(mode, func(t *testing.T) {
			f, s := codexFixture(t)
			at := f.now.UnixMilli()
			if r := f.hookAs(s, codexServerPrompt(mode, nil, "t1", at+10, true)); r.Enrollment != "joined" {
				t.Fatalf("recovering prompt: %+v, want joined", r)
			}
			if got := f.launches(s); len(got) != 1 || got[0] != (launchRow{SessionLaunch, false, 0, "unverified"}) {
				t.Fatalf("launches %+v", got)
			}
			if route, found := f.codexRoute(s); found {
				t.Fatalf("a prompt without its server registered %+v", route)
			}
		})
	}
}

// A member recovered without its server, before prompts named it or by a
// prompt that did not, gets the route from its next prompt that does.
func TestARecoveredCodexMemberGetsItsRouteFromALaterPrompt(t *testing.T) {
	f, s := codexFixture(t)
	at := f.now.UnixMilli()
	f.hookAs(s, codexServerPrompt(publish.ModeDaemon, nil, "t1", at+10, true))
	f.hookAs(s, codexServerPrompt(publish.ModeDaemon, nil, "t2", at+20, false))
	if _, found := f.codexRoute(s); found {
		t.Fatal("a prompt without its server registered a route")
	}
	server := codexServer
	f.hookAs(s, codexServerPrompt(publish.ModeDaemon, &server, "t3", at+30, false))
	route, found := f.codexRoute(s)
	if !found || route.mode != publish.ModeDaemon || route.state != "active" || route.hostPID != codexServer.PID {
		t.Fatalf("route %+v (found %t), want the daemon route", route, found)
	}
}

// A prompt never changes a route that exists: one a start registered keeps
// its server, and one that ended or was invalidated is not revived.
func TestACodexPromptLeavesAnExistingRouteAsItIs(t *testing.T) {
	for _, state := range []string{"active", "ended", "invalidated"} {
		t.Run(state, func(t *testing.T) {
			f, s := codexFixture(t)
			at := f.now.UnixMilli()
			f.hook(s, codexStart("startup", publish.ModeDaemon, at))
			if _, err := f.db.Exec(`UPDATE publish_route SET state = ? WHERE binding_id = ?`, state,
				bindingIDOf(t, f, f.principalOf(s))); err != nil {
				t.Fatal(err)
			}
			before, _ := f.codexRoute(s)
			other := wire.Host{PID: 5151, Start: 77}
			f.hookAs(s, codexServerPrompt(publish.ModeDaemon, &other, "t1", at+10, false))
			if after, _ := f.codexRoute(s); after != before {
				t.Fatalf("route %+v became %+v", before, after)
			}
		})
	}
}

// Claude's route rests on its launch: a recovering Claude prompt that names
// a server (no Claude hook does) registers nothing from it.
func TestAClaudePromptRegistersNoRouteFromAServer(t *testing.T) {
	f, _, _ := lateFixture(t)
	s := f.newSession("claude")
	at := f.now.UnixMilli()
	cmd := recoveryPrompt(SessionLaunch, "t1", at+10)
	server := codexServer
	cmd.Host, cmd.VendorHome = &server, codexHome
	f.hookAs(s, cmd)
	if got := f.launches(s); len(got) != 1 || got[0].mode != "unverified" {
		t.Fatalf("launches %+v, want today's unverified session launch", got)
	}
	if f.routeLaunch(s) != "" {
		t.Fatal("a Claude prompt registered a route from a server")
	}
}

// Only a prompt registers a route, and only for the binding its leaf
// authenticated: a Stop that names a server, and a prompt from a leaf of
// another binding, register nothing.
func TestOnlyAnAuthenticatedPromptRegistersACodexRoute(t *testing.T) {
	f, s := codexFixture(t)
	other := f.newSession("codex")
	at := f.now.UnixMilli()
	f.hookAs(s, codexServerPrompt(publish.ModeDaemon, nil, "t1", at+10, true))
	f.hookAs(other, codexServerPrompt(publish.ModeDaemon, nil, "o1", at+15, true))
	server := codexServer
	finishing := codexServerPrompt(publish.ModeDaemon, &server, "t1", at+20, false)
	finishing.Fact, finishing.EventRef = FactTurnFinishing, "t1:stop"
	f.hookAs(s, finishing)
	if _, found := f.codexRoute(s); found {
		t.Fatal("a Stop registered a route")
	}
	caller := f.caller(s)
	otherBinding := bindingIDOf(t, f, f.principalOf(other))
	caller.BindingID = &otherBinding
	cmd := codexServerPrompt(publish.ModeDaemon, &server, "t2", at+30, false)
	f.exec(caller, Request{Command: cmd, Vendor: s.vendor, SessionKey: s.key, OpKey: hookKey(cmd)})
	if _, found := f.codexRoute(s); found {
		t.Fatal("a prompt from another binding's leaf registered a route")
	}
}
