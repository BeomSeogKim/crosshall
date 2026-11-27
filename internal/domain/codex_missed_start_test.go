package domain

import (
	"context"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// #252, Codex: a session whose start the dispatcher missed gets, from the
// recovering prompt when it names its server, the launch start, mode and
// route that start would have given. Nothing else registers a route from a
// prompt.

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
	if got := f.launches(s); len(got) != 1 || got[0] != (launchRow{SessionLaunch, false, at + 10, publish.ModeDaemon}) {
		t.Fatalf("launches %+v, want the session launch, started at the prompt, in the prompt's mode", got)
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

// The route a recovering prompt registers carries notices: it is proven on
// the server the prompt named, which started before the prompt, activates
// and reserves a notice like the route of a session whose start arrived.
func TestARouteARecoveringCodexPromptRegisteredCarriesANotice(t *testing.T) {
	useCodexSender(t)
	f, writer := adapterChannel(t)
	f.policy("codex", true, 1)
	s := f.newSession("codex")
	at := f.now.UnixMilli()
	server := codexServer
	f.hookAs(s, codexServerPrompt(publish.ModeDaemon, &server, "t1", at+10, true))
	codexEvidence(t, f, "0.171.0")
	binding := bindingIDOf(t, f, f.principalOf(s))
	proofs := inTx(t, f, func(ctx context.Context, tx store.Tx) ([]PublishProof, error) {
		return UnprovenPublishRoutes(ctx, tx, f.now)
	})
	if len(proofs) != 1 || proofs[0].Probe.StartedHookAtMS != at+10 {
		t.Fatalf("unproven routes %+v, want this route judged against the prompt's time", proofs)
	}
	queue := publish.Queue{PID: codexServer.PID, StartMS: codexServer.Start}
	if inTx(t, f, func(ctx context.Context, tx store.Tx) (bool, error) {
		return ConfirmPublishProof(ctx, tx, proofs[0], publish.Proven{Queue: queue, StartedAtMS: at + 11})
	}) {
		t.Fatal("a server that started after the prompt was recorded")
	}
	proveCodexRoutes(t, f)
	if got := publishStateOf(t, f, binding); got.State != "available" {
		t.Fatalf("the proven route reads %s (%s)", got.State, got.Reason)
	}
	var alias string
	if err := f.db.QueryRow(`SELECT alias FROM binding WHERE id = ?`, binding).Scan(&alias); err != nil {
		t.Fatal(err)
	}
	// The recovering prompt's turn runs until its Stop; a notice waits for it.
	finishing := codexServerPrompt(publish.ModeDaemon, nil, "t1", at+20, false)
	finishing.Fact, finishing.EventRef = FactTurnFinishing, "t1:stop"
	f.hookAs(s, finishing)
	f.turnEndAsk(writer, alias, "to-recovered")
	if !codexReserve(t, f, binding) {
		t.Fatal("the recovered route reserved no notice")
	}
	if got := publishStateOf(t, f, binding); got.State != "held" {
		t.Fatalf("after its notice the route reads %s", got.State)
	}
}

// A member recovered without its server gets its route from its next
// prompt that names the server, as a hand-joined member does (#437); its
// next start replaces it as any start does.
func TestARecoveredCodexMemberGetsItsRouteFromItsNextNamingPrompt(t *testing.T) {
	f, s := codexFixture(t)
	at := f.now.UnixMilli()
	f.hookAs(s, codexServerPrompt(publish.ModeDaemon, nil, "t1", at+10, true))
	if route, found := f.codexRoute(s); found {
		t.Fatalf("a prompt without its server registered %+v", route)
	}
	server := codexServer
	f.hookAs(s, codexServerPrompt(publish.ModeDaemon, &server, "t2", at+20, false))
	if route, found := f.codexRoute(s); !found || route.state != "active" || route.hostPID != codexServer.PID || route.thread != s.key {
		t.Fatalf("after a prompt that names the server the route is %+v (found %t)", route, found)
	}
	if got := f.launches(s); len(got) != 1 || got[0] != (launchRow{SessionLaunch, false, at + 20, publish.ModeDaemon}) {
		t.Fatalf("launches %+v, want the session launch started at the naming prompt", got)
	}
	f.hook(s, codexStart("resume", publish.ModeDaemon, at+30))
	if route, found := f.codexRoute(s); !found || route.state != "active" || route.hostPID != codexServer.PID {
		t.Fatalf("after its next start the route is %+v (found %t)", route, found)
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
