package domain

import (
	"context"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// #437, Codex: a session that joined by hand while its start was missed
// gets, from its first prompt or turn end that names its server, the route
// that start would have registered, under the recovering prompt's trust
// rule (#355).

// codexServerTurnEnd is a Codex turn end of the session launch, naming the
// managed server when server is set.
func codexServerTurnEnd(server *wire.Host, turn string, at int64) HookEvent {
	cmd := codexServerPrompt(publish.ModeDaemon, server, turn, at, false)
	cmd.Fact, cmd.EventRef = FactTurnFinishing, turn+":stop"
	return cmd
}

func handJoinedCodex(t *testing.T) (*fixture, session, session) {
	t.Helper()
	f, writer := adapterChannel(t)
	f.policy("codex", true, 1)
	s := f.newSession("codex")
	wantOK(t, f.join(s, Join{Channel: "checkout", Alias: "codex1"}, ""))
	if route, found := f.codexRoute(s); found {
		t.Fatalf("a hand join registered %+v", route)
	}
	return f, s, writer
}

func TestAHandJoinedCodexMemberGetsItsRouteFromANamingCallback(t *testing.T) {
	for _, tc := range []struct {
		name  string
		event func(*wire.Host, string, int64) HookEvent
	}{
		{"turn end", codexServerTurnEnd},
		{"prompt", func(h *wire.Host, turn string, at int64) HookEvent {
			return codexServerPrompt(publish.ModeDaemon, h, turn, at, false)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, s, _ := handJoinedCodex(t)
			at := f.now.UnixMilli()
			f.hookAs(s, tc.event(nil, "t1", at+10))
			if route, found := f.codexRoute(s); found {
				t.Fatalf("a callback without its server registered %+v", route)
			}
			server := codexServer
			f.hookAs(s, tc.event(&server, "t2", at+20))
			route, found := f.codexRoute(s)
			if !found || route.mode != publish.ModeDaemon || route.state != "active" || route.hostPID != codexServer.PID ||
				route.home != codexHome || route.thread != s.key {
				t.Fatalf("route %+v (found %t), want the daemon route the start would register", route, found)
			}
			if got := f.launches(s); len(got) != 1 || got[0] != (launchRow{SessionLaunch, false, at + 20, publish.ModeDaemon}) {
				t.Fatalf("launches %+v, want the session launch started at the naming callback", got)
			}
			other := wire.Host{PID: 5151, Start: 77}
			f.hookAs(s, tc.event(&other, "t3", at+30))
			if after, _ := f.codexRoute(s); after != route {
				t.Fatalf("a later naming callback changed the route %+v to %+v", route, after)
			}
		})
	}
}

// A session whose start was seen keeps the route that start registered: a
// turn end that names another server changes nothing.
func TestACodexTurnEndLeavesAStartedRouteAsItIs(t *testing.T) {
	f, s := codexFixture(t)
	at := f.now.UnixMilli()
	f.hook(s, codexStart("startup", publish.ModeDaemon, at))
	before, found := f.codexRoute(s)
	if !found {
		t.Fatal("the start registered no route")
	}
	other := wire.Host{PID: 5151, Start: 77}
	f.hookAs(s, codexServerTurnEnd(&other, "t1", at+10))
	if after, _ := f.codexRoute(s); after != before {
		t.Fatalf("route %+v became %+v", before, after)
	}
}

// Claude's route rests on its launch: a turn end that names a server (no
// Claude hook does) registers nothing from it.
func TestAClaudeTurnEndRegistersNoRouteFromAServer(t *testing.T) {
	f, _ := adapterChannel(t)
	s := f.newSession("claude")
	wantOK(t, f.join(s, Join{Channel: "checkout", Alias: "claude1"}, ""))
	cmd := stop(SessionLaunch, "t1", f.now.UnixMilli()+10)
	server := codexServer
	cmd.Host, cmd.VendorHome = &server, codexHome
	f.hookAs(s, cmd)
	if f.routeLaunch(s) != "" {
		t.Fatal("a Claude turn end registered a route from a server")
	}
}

// The route a turn end registers carries notices: it is proven on the
// server the turn end named, judged against the turn end's time, and
// reserves a notice with no prompt after the join.
func TestARouteAHandJoinedCodexTurnEndRegisteredCarriesANotice(t *testing.T) {
	useCodexSender(t)
	f, s, writer := handJoinedCodex(t)
	at := f.now.UnixMilli()
	server := codexServer
	f.hookAs(s, codexServerTurnEnd(&server, "t1", at+10))
	codexEvidence(t, f, "0.171.0")
	binding := bindingIDOf(t, f, f.principalOf(s))
	proofs := inTx(t, f, func(ctx context.Context, tx store.Tx) ([]PublishProof, error) {
		return UnprovenPublishRoutes(ctx, tx, f.now)
	})
	if len(proofs) != 1 || proofs[0].Probe.StartedHookAtMS != at+10 {
		t.Fatalf("unproven routes %+v, want this route judged against the turn end's time", proofs)
	}
	queue := publish.Queue{PID: codexServer.PID, StartMS: codexServer.Start}
	if inTx(t, f, func(ctx context.Context, tx store.Tx) (bool, error) {
		return ConfirmPublishProof(ctx, tx, proofs[0], publish.Proven{Queue: queue, StartedAtMS: at + 11})
	}) {
		t.Fatal("a server that started after the turn end was recorded")
	}
	proveCodexRoutes(t, f)
	if got := publishStateOf(t, f, binding); got.State != "available" {
		t.Fatalf("the proven route reads %s (%s)", got.State, got.Reason)
	}
	f.turnEndAsk(writer, "codex1", "to-hand-joined")
	if !codexReserve(t, f, binding) {
		t.Fatal("the hand-joined route reserved no notice")
	}
}

// A turn end captured before a hand rejoin belongs to the membership the
// rejoin replaced: it registers nothing and does not start the launch, so
// the first naming callback after the rejoin still registers the route.
// A turn end in the same millisecond as the rejoin is as ambiguous as it is
// for the selection fence, and is treated as one from before it.
func TestACodexCallbackFromBeforeARejoinLeavesTheRouteToALaterOne(t *testing.T) {
	for _, tc := range []struct {
		name   string
		offset int64
	}{{"before", 20}, {"tied", 40}} {
		t.Run(tc.name, func(t *testing.T) {
			f, s, _ := handJoinedCodex(t)
			at := f.now.UnixMilli()
			wantOK(t, f.run(s, Leave{}, ""))
			f.now = time.UnixMilli(at + 40)
			wantOK(t, f.join(s, Join{Channel: "checkout", Alias: "codex1"}, ""))
			stale := wire.Host{PID: 5151, Start: 77}
			f.hookAs(s, codexServerTurnEnd(&stale, "t1", at+tc.offset))
			if route, found := f.codexRoute(s); found {
				t.Fatalf("a turn end from before the rejoin registered %+v", route)
			}
			for _, l := range f.launches(s) {
				if l.started > 0 {
					t.Fatalf("a turn end from before the rejoin started the launch: %+v", l)
				}
			}
			server := codexServer
			f.hookAs(s, codexServerPrompt(publish.ModeDaemon, &server, "t2", at+60, false))
			route, found := f.codexRoute(s)
			if !found || route.state != "active" || route.hostPID != codexServer.PID {
				t.Fatalf("after a naming prompt past the rejoin the route is %+v (found %t)", route, found)
			}
		})
	}
}

// A member whose start was seen before it left, and that rejoined by hand,
// gets its route from a naming turn end after the rejoin: the old start and
// the ended route belong to the old membership.
func TestACodexMemberRejoinedByHandGetsItsRouteFromATurnEnd(t *testing.T) {
	f, s := codexFixture(t)
	at := f.now.UnixMilli()
	f.hook(s, codexStart("startup", publish.ModeDaemon, at))
	wantOK(t, f.run(s, Leave{}, ""))
	f.now = time.UnixMilli(at + 40)
	wantOK(t, f.join(s, Join{Channel: "checkout", Alias: "codex1"}, ""))
	server := codexServer
	f.hookAs(s, codexServerTurnEnd(&server, "t1", at+60))
	var epoch int64
	var state string
	var host int64
	if err := f.db.QueryRow(`SELECT r.binding_epoch, r.state, COALESCE(r.host_pid, 0) FROM publish_route r JOIN binding b ON b.id = r.binding_id
		WHERE b.principal_id = ?`, f.principalOf(s)).Scan(&epoch, &state, &host); err != nil || epoch != 2 || state != "active" || host != codexServer.PID {
		t.Fatalf("route at epoch %d %s host %d (%v), want the server's route at epoch 2", epoch, state, host, err)
	}
}

// A named server the dispatcher cannot judge, such as one in another PID
// namespace, registers nothing and starts no launch, so a later callback
// that names a usable server still can.
func TestAnUnjudgeableCodexServerLeavesTheRouteToALaterCallback(t *testing.T) {
	f, s, _ := handJoinedCodex(t)
	at := f.now.UnixMilli()
	foreign := codexServer
	foreign.NS = "another-namespace"
	f.hookAs(s, codexServerTurnEnd(&foreign, "t1", at+10))
	if route, found := f.codexRoute(s); found {
		t.Fatalf("an unjudgeable server registered %+v", route)
	}
	for _, l := range f.launches(s) {
		if l.started > 0 {
			t.Fatalf("an unjudgeable server started the launch: %+v", l)
		}
	}
	server := codexServer
	f.hookAs(s, codexServerTurnEnd(&server, "t2", at+20))
	if route, found := f.codexRoute(s); !found || route.state != "active" || route.hostPID != codexServer.PID {
		t.Fatalf("after a usable server the route is %+v (found %t)", route, found)
	}
}
