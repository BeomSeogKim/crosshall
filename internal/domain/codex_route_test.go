package domain

import (
	"database/sql"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/notice"
	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func codexProfile(mode string) wire.HookProfile {
	return wire.HookProfile{Adapter: "codex", ManifestRev: "codex-1", VendorVersion: "0.171.0", Platform: "darwin-arm64", Mode: mode}
}

// codexServer is the managed server Codex daemon starts name as their
// host, and codexHome the home it serves.
var codexServer = wire.Host{PID: 4242, Start: 99}

const codexHome = "/home/fixture/.codex"

// codexStart is a Codex SessionStart: always session-scoped, with the mode
// its hook read from the process that runs it, and in daemon mode that
// server and its home.
func codexStart(source, mode string, at int64) HookEnroll {
	cmd := start(SessionLaunch, source, at)
	cmd.Profile = codexProfile(mode)
	if mode == publish.ModeDaemon {
		host := codexServer
		cmd.Host, cmd.VendorHome = &host, codexHome
	}
	return cmd
}

func codexEnded(mode string, at int64) HookEvent {
	cmd := ended(SessionLaunch, at)
	cmd.Profile = codexProfile(mode)
	return cmd
}

// A Codex route carries the mode of the start that registered it: every
// start decides again, a SessionEnd closes the route, and the owner reads
// why a session outside the managed server cannot be reached (#236).
func TestCodexRouteFollowsTheModeOfEachStart(t *testing.T) {
	f, _ := adapterChannel(t)
	f.policy("codex", true, 1)
	s := f.newSession("codex")
	t0 := f.now.UnixMilli()
	reason := func(bid int64) (string, string) {
		t.Helper()
		state, found, err := store.ReadPublishState(t.Context(), f.db, bid, f.now)
		if err != nil || !found {
			t.Fatalf("publish state: %+v %v %v", state, found, err)
		}
		return state.State, state.Reason
	}

	f.hook(s, codexStart("startup", publish.ModeDaemon, t0))
	bid := bindingIDOf(t, f, f.principalOf(s))
	gen, sess, launch, mode, state, _, _ := routeState(t, f, bid)
	if sess != s.key || launch != SessionLaunch || mode != publish.ModeDaemon || state != "active" {
		t.Fatalf("daemon start: session %q launch %q mode %q state %q", sess, launch, mode, state)
	}
	// Codex sends now; without tested evidence for the build the route
	// stays a candidate.
	if got, why := reason(bid); got != "candidate" || why != "no tested evidence for codex-1 on darwin-arm64 daemon" {
		t.Fatalf("daemon start reads %s (%s)", got, why)
	}

	f.hook(s, codexEnded(publish.ModeDaemon, t0+1_000))
	genEnded, _, _, _, state, why, _ := routeState(t, f, bid)
	if genEnded <= gen || state != "ended" || why != "session ended" {
		t.Fatalf("after SessionEnd: gen %d state %q reason %q", genEnded, state, why)
	}

	for i, tc := range []struct{ mode, reason string }{
		{publish.ModeInProcess, publish.CodexNoDaemon},
		{"unverified", publish.CodexDaemonUnverified},
		{publish.ModeDaemon, "no tested evidence for codex-1 on darwin-arm64 daemon"},
	} {
		at := t0 + int64(2_000*(i+1))
		f.hook(s, codexStart("resume", tc.mode, at))
		next, _, _, mode, state, _, _ := routeState(t, f, bid)
		if next <= gen || mode != tc.mode || state != "active" {
			t.Fatalf("resume %s: gen %d mode %q state %q", tc.mode, next, mode, state)
		}
		gen = next
		if got, why := reason(bid); got != "candidate" || why != tc.reason {
			t.Fatalf("resume %s reads %s (%s), want candidate (%s)", tc.mode, got, why, tc.reason)
		}
		f.hook(s, codexEnded(tc.mode, at+1_000))
	}
}

// codexAttempt records a submitted notice to a Codex member's thread as a
// sender would: on the route's current generation, for the given asks.
func codexAttempt(t *testing.T, f *fixture, bid, principal int64, ids []int64) int64 {
	t.Helper()
	res, err := f.db.Exec(`INSERT INTO publish_attempt (binding_id, principal_id, binding_epoch, generation, adapter,
		session_ref, launch_ref, policy_epoch, process_pid, process_start_ms, lane, state, reserved_at_ms, submitting_at_ms)
		SELECT binding_id, principal_id, binding_epoch, generation, adapter, session_ref, launch_ref, policy_epoch,
			42, 1, `+store.PublishThreadLane()+`, 'submitted', ?1, ?1 FROM publish_route r WHERE binding_id = ?2`, f.now.UnixMilli(), bid)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	for _, msg := range ids {
		if _, err := f.db.Exec(`INSERT INTO publish_attempt_item (attempt_id, principal_id, message_id) VALUES (?, ?, ?)`,
			id, principal, msg); err != nil {
			t.Fatal(err)
		}
	}
	return id
}

func codexReceipt(turn string, at int64, ids []int64) HookEvent {
	cmd := prompt(SessionLaunch, turn, at)
	cmd.Profile = codexProfile(publish.ModeDaemon)
	codex, _ := publish.Lookup("codex")
	line, _ := codex.Notice(ids)
	echoed, _ := codex.Echo(line)
	cmd.Receipt = notice.Receipt(echoed)
	return cmd
}

// A Codex launch is session-scoped, so its notice's receipt is matched on
// the lane it was queued to, the thread in its home, with the exact ID list
// as for any receipt (#236). A new route generation (a compaction or a
// resume) does not make it stale. A receipt from that lane after the owner
// changed the policy frees the lane but is not judged; one from another home
// settles nothing.
func TestCodexReceiptIsMatchedOnTheThread(t *testing.T) {
	for _, tc := range []struct {
		name        string
		change      string // applied to the route after the notice was sent
		malformed   bool
		observation string
		rejected    string
		settled     bool
	}{
		{name: "right thread", observation: "own_turn"},
		{name: "route on another thread", change: `UPDATE publish_route SET thread_id = 'another-thread'`},
		{name: "older generation", change: `UPDATE publish_route SET generation = generation + 1`, observation: "own_turn"},
		{name: "another home", change: `UPDATE publish_route SET vendor_home = '/home/other/.codex'`, rejected: "stale"},
		{name: "policy changed", change: `UPDATE publish_route SET policy_epoch = policy_epoch + 1`, rejected: "stale", settled: true},
		{name: "malformed", malformed: true, rejected: "malformed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, writer := adapterChannel(t)
			f.policy("codex", true, 1)
			reader := f.newSession("codex")
			wantOK(t, f.join(reader, Join{Channel: "checkout", Alias: "reader"}, "reader-join"))
			f.hook(reader, codexStart("startup", publish.ModeDaemon, f.now.UnixMilli()))
			principal := f.principalOf(reader)
			bid := bindingIDOf(t, f, principal)
			ask := f.turnEndAsk(writer, "reader", "codex-receipt")
			attempt := codexAttempt(t, f, bid, principal, []int64{ask})
			if tc.change != "" {
				if _, err := f.db.Exec(tc.change+` WHERE binding_id = ?`, bid); err != nil {
					t.Fatal(err)
				}
			}
			cmd := codexReceipt("notice", f.now.UnixMilli()+100, []int64{ask})
			if tc.malformed {
				cmd.Receipt = "publish:x"
			}
			f.hook(reader, cmd)
			var observation sql.NullString
			if err := f.db.QueryRow(`SELECT observation FROM publish_attempt WHERE id = ?`, attempt).Scan(&observation); err != nil {
				t.Fatal(err)
			}
			var rejected sql.NullString
			_ = f.db.QueryRow(`SELECT json_extract(detail, '$.reason') FROM event WHERE kind = 'publish_receipt_rejected'`).Scan(&rejected)
			if observation.String != tc.observation || rejected.String != tc.rejected {
				t.Fatalf("observation %q, rejected %q; want %q, %q", observation.String, rejected.String, tc.observation, tc.rejected)
			}
			var retired sql.NullInt64
			if err := f.db.QueryRow(`SELECT retired_at_ms FROM publish_attempt WHERE id = ?`, attempt).Scan(&retired); err != nil {
				t.Fatal(err)
			}
			var settled int
			_ = f.db.QueryRow(`SELECT COUNT(*) FROM event WHERE kind = 'publish_attempt_settled'
				AND json_extract(detail, '$.reason') = 'stale receipt'`).Scan(&settled)
			if retired.Valid != tc.settled || settled != map[bool]int{true: 1}[tc.settled] {
				t.Fatalf("retired %v with %d settlements, want settled %v", retired, settled, tc.settled)
			}
			var probation int
			_ = f.db.QueryRow(`SELECT COUNT(*) FROM capability_build WHERE confirmed > 0 OR stop_ref IS NOT NULL`).Scan(&probation)
			if tc.settled && probation != 0 {
				t.Fatal("a settled receipt fed build evidence")
			}
		})
	}
}
