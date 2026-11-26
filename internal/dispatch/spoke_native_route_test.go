package dispatch

import (
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/hostclock"
	"github.com/BeomSeogKim/crosshall/internal/link"
	"github.com/BeomSeogKim/crosshall/internal/proc"
	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func TestSpokeNativeRouteKeepsCodexAddressLocalAndFenced(t *testing.T) {
	f := newHookFixture(t)
	defer f.end(t, f.home)
	const home = "/configure/codex"
	const started = 1000
	host := wire.Host{PID: 123, Start: 456, NS: proc.PIDNamespace()}
	got := f.hook(t, wire.VerbHookEnroll, wire.HookEnrollArgs{
		Profile:   wire.HookProfile{Adapter: "codex", Mode: publish.ModeDaemon},
		LaunchRef: hookLaunch, EventRef: "start", Host: &host, VendorHome: home, Epoch: 3, HookAtMS: started,
	}, 9, stamp22)
	if got.Status != core.ExitOK {
		t.Fatalf("enroll: %+v", got)
	}
	var route spokeNativeRoute
	read := func(boot string, want bool) {
		t.Helper()
		if err := f.m.db.View(t.Context(), func(tx store.Tx) error {
			mono, err := f.e.mono()
			if err != nil {
				return err
			}
			routes, err := readSpokeNativeRoutes(t.Context(), tx, boot, mono)
			if err != nil {
				return err
			}
			var eligible bool
			route, eligible, err = readSpokeNativeRoute(t.Context(), tx, boot, eventMember)
			if eligible != want || len(routes) != 0 && !want || want && (len(routes) != 1 || routes[0] != route) {
				t.Fatalf("route: %+v, eligible %v, listing %+v; want presence %v", route, eligible, routes, want)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	read(f.e.boot, true)
	if probe := route.Probe; probe.Adapter != "codex" || probe.Address != eventMember.Key || probe.Route.Home != home ||
		probe.Route.Host != (publish.Queue{PID: host.PID, StartMS: host.Start}) || probe.Home != "" ||
		probe.StartedHookAtMS != started || route.Scope != f.scope(2, 2) || route.CaptureSeq != 9 {
		t.Fatalf("route: %+v", route)
	}
	if err := f.m.db.View(t.Context(), func(tx store.Tx) error {
		var body string
		if err := tx.QueryRowContext(t.Context(), `SELECT body FROM spoke_hook_outbox`).Scan(&body); err != nil {
			return err
		}
		if strings.Contains(body, home) || strings.Contains(body, "pid_namespace") || strings.Contains(body, `"host"`) {
			t.Fatalf("local route was sent to the home: %s", body)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	read("previous-boot", false)
	f.exec(t, `UPDATE spoke_policy SET enabled = 0 WHERE adapter = 'codex'`)
	read(f.e.boot, false)
	f.exec(t, `UPDATE spoke_policy SET enabled = 1 WHERE adapter = 'codex'`)
	f.exec(t, `UPDATE spoke_event_cursor SET eligible = 0`)
	read(f.e.boot, false)
	f.exec(t, `UPDATE spoke_event_cursor SET eligible = 1`)
	f.exec(t, `UPDATE spoke_leaf SET membership_gen = 3 WHERE vendor = 'codex'`)
	read(f.e.boot, false)
}

func TestSpokeNativeRouteRequiresFreshClaudeCaptureForANewRouteEpoch(t *testing.T) {
	db, _ := openSpokeWakeStore(t)
	ctx := t.Context()
	member := link.Member{Vendor: "claude", Key: "client"}
	token := link.HookToken{Boot: "boot", LeafGeneration: "gen", Membership: link.Membership{BindingID: 4, Generation: 2}}
	const firstSocket = "/tmp/cc-socks/first.sock"
	const secondSocket = "/tmp/cc-socks/second.sock"
	write := func(sql string) {
		t.Helper()
		if err := db.Update(ctx, func(tx store.Tx) error {
			_, err := tx.ExecContext(ctx, sql)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	write(`INSERT INTO spoke_leaf(digest, vendor, key, generation, epoch, file_identity, created_at_ms, binding_id, membership_gen)
		VALUES ('digest', 'claude', 'client', 'gen', 1, 'file', 1, 4, 2);
		INSERT INTO spoke_policy VALUES ('claude', 1, 1, 3);
		INSERT INTO spoke_event_cursor(id, boot, acked_seq, eligible) VALUES (1, 'boot', 5, 1)`)
	check := func(want string, seq uint64) {
		t.Helper()
		if err := db.View(ctx, func(tx store.Tx) error {
			route, ok, err := readSpokeNativeRoute(ctx, tx, token.Boot, member)
			if err == nil && ((want != "") != ok || ok && (route.Probe.Address != want || route.CaptureSeq != seq)) {
				t.Fatalf("route %+v, %v; want %q at %d", route, ok, want, seq)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	capture := func(seq uint64, material spokeNativeHook) {
		t.Helper()
		token.LeafSeq = seq
		if err := db.Update(ctx, func(tx store.Tx) error {
			return recordSpokeNativeHook(ctx, tx, member, token, "launch", 3, material)
		}); err != nil {
			t.Fatal(err)
		}
	}
	start := spokeNativeHook{enroll: true, startedAtMS: 1, verified: true, pidNamespace: proc.PIDNamespace()}
	socket := func(path, mode string, start bool) spokeNativeHook {
		return spokeNativeHook{report: &wire.HookRouteArgs{SocketPath: path, PermissionMode: mode, Start: start}}
	}
	capture(6, start)
	capture(7, socket(firstSocket, "default", true))
	check("", 0)
	write(`INSERT INTO spoke_member_scope (vendor, key, leaf_generation, binding_id, membership_gen, route_epoch, cut_seq)
		VALUES ('claude', 'client', 'gen', 4, 2, 1, 5)`)
	check(firstSocket, 7)
	capture(8, socket(secondSocket, "default", false))
	check(firstSocket, 7)
	capture(9, socket("", "bypass", false))
	check("", 0)
	capture(10, socket(secondSocket, "default", true))
	check(secondSocket, 10)
	mono, err := hostclock.Mono()
	if err != nil {
		t.Fatal(err)
	}
	token.MonoNS = mono
	capture(11, spokeNativeHook{fact: wire.HookApprovalRequested})
	check("", 0)
	token.MonoNS += int64(time.Millisecond)
	capture(12, spokeNativeHook{fact: wire.HookTurnFinishing})
	check(secondSocket, 12)
	write(`UPDATE spoke_member_scope SET route_epoch = 2, cut_seq = 13`)
	check("", 0)
	capture(13, start)
	capture(14, socket(secondSocket, "default", true))
	check("", 0)
	capture(15, start)
	check(secondSocket, 15)
	capture(16, spokeNativeHook{fact: wire.HookSessionEnded})
	check("", 0)
}
