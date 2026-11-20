package domain

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func claudeRoute(launch, eventRef, socketPath, mode string, atMS int64, epoch uint64) HookRoute {
	return HookRoute{wire.HookRouteArgs{
		Adapter:        "claude",
		LaunchRef:      launch,
		LaunchVerified: true,
		EventRef:       eventRef,
		HookAtMS:       atMS,
		Epoch:          epoch,
		PermissionMode: mode,
		SocketPath:     socketPath,
	}}
}

func enrollAndRoute(t *testing.T, f *fixture, s session, launch, source, socketPath, mode string, atMS int64, epoch uint64) {
	t.Helper()
	cmd := start(launch, source, atMS)
	cmd.Epoch = epoch
	f.hook(s, cmd)
	r := f.exec(f.caller(s), Request{
		Command:    claudeRoute(launch, fmt.Sprintf("route:%s:%d", source, atMS), socketPath, mode, atMS, epoch),
		Vendor:     s.vendor,
		SessionKey: s.key,
		OpKey:      fmt.Sprintf("hook:claude:%s:route:%s:%d", launch, source, atMS),
	})
	wantOK(t, r)
}

func routeState(t *testing.T, f *fixture, bindingID int64) (gen int64, sessionRef, launchRef, mode, state, reason string, watermark int64) {
	t.Helper()
	err := f.db.QueryRow(`SELECT generation, session_ref, launch_ref, mode, state, reason, watermark_message_id
		FROM publish_route WHERE binding_id = ?`, bindingID).
		Scan(&gen, &sessionRef, &launchRef, &mode, &state, &reason, &watermark)
	if err != nil {
		t.Fatalf("query publish_route: %v", err)
	}
	return
}

func bindingIDOf(t *testing.T, f *fixture, principal int64) int64 {
	t.Helper()
	var id int64
	if err := f.db.QueryRow(`SELECT id FROM binding WHERE principal_id = ? ORDER BY id DESC LIMIT 1`, principal).Scan(&id); err != nil {
		t.Fatalf("query binding: %v", err)
	}
	return id
}

func TestPublishRouteClearAndResumeAdvanceGenerationAndWatermark(t *testing.T) {
	f, peer := adapterChannel(t)
	reader := f.newSession("codex")
	f.join(reader, Join{Channel: "checkout", Alias: "reader"}, "join-reader")
	if r := f.run(peer, Ask{To: []string{"@reader"}, Body: "before-enroll"}, "ask-1"); r.Status != core.ExitOK {
		t.Fatalf("ask: %+v", r)
	}
	var msg1 int64
	if err := f.db.QueryRow(`SELECT MAX(id) FROM message`).Scan(&msg1); err != nil {
		t.Fatal(err)
	}

	s := f.newSession("claude")
	t0 := f.now.UnixMilli()
	enrollAndRoute(t, f, s, "pid-1", "startup", "/tmp/cc-socks/1.sock", "default", t0, 1)
	bid := bindingIDOf(t, f, f.principalOf(s))
	gen, sess, _, _, state, _, wm := routeState(t, f, bid)
	if sess != s.key || state != "active" || wm != msg1 {
		t.Fatalf("after startup: gen=%d sess=%q state=%q wm=%d (want sess=%q, active, %d)", gen, sess, state, wm, s.key, msg1)
	}

	if r := f.run(peer, Note{To: []string{"@reader"}, Body: "before-clear"}, "note-1"); r.Status != core.ExitOK {
		t.Fatalf("note: %+v", r)
	}
	var msg2 int64
	_ = f.db.QueryRow(`SELECT MAX(id) FROM message`).Scan(&msg2)

	f.hook(s, ended("pid-1", t0+1_000))
	genAfterEnd, _, _, _, state, reason, _ := routeState(t, f, bid)
	if genAfterEnd <= gen || state != "ended" || reason != "session ended" {
		t.Fatalf("after SessionEnd(clear): gen=%d state=%q reason=%q", genAfterEnd, state, reason)
	}

	enrollAndRoute(t, f, s, "pid-1", "clear", "/tmp/cc-socks/1.sock", "default", t0+2_000, 1)
	genAfterClear, _, _, _, state, _, wm := routeState(t, f, bid)
	if genAfterClear <= genAfterEnd || state != "active" || wm != msg2 {
		t.Fatalf("after SessionStart(clear): gen=%d state=%q wm=%d (want >%d, active, %d)", genAfterClear, state, wm, genAfterEnd, msg2)
	}

	f.hook(s, ended("pid-1", t0+3_000))
	enrollAndRoute(t, f, s, "pid-1", "resume", "/tmp/cc-socks/1.sock", "default", t0+4_000, 1)
	genAfterResume, _, _, _, state, _, _ := routeState(t, f, bid)
	if genAfterResume <= genAfterClear || state != "active" {
		t.Fatalf("after resume: gen=%d state=%q", genAfterResume, state)
	}
}

func TestPublishRouteModeChangeOrderingAndIncompleteRoute(t *testing.T) {
	f, _ := adapterChannel(t)
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()
	enrollAndRoute(t, f, s, "pid-1", "startup", "/tmp/cc-socks/1.sock", "default", t0, 1)
	bid := bindingIDOf(t, f, f.principalOf(s))
	gen0, _, _, _, _, _, _ := routeState(t, f, bid)

	sendMode := func(ref, mode string, at int64) {
		r := f.exec(f.caller(s), Request{
			Command:    claudeRoute("pid-1", ref, "", mode, at, 1),
			Vendor:     s.vendor,
			SessionKey: s.key,
			OpKey:      "hook:claude:pid-1:" + ref,
		})
		wantOK(t, r)
	}

	sendMode("m1", "acceptEdits", t0+1_000)
	gen, _, _, mode, state, _, _ := routeState(t, f, bid)
	if gen != gen0 || mode != "acceptEdits" || state != "active" {
		t.Fatalf("same-side mode change: gen=%d mode=%q state=%q", gen, mode, state)
	}

	sendMode("m2", "bypassPermissions", t0+3_000)
	gen, _, _, mode, state, reason, _ := routeState(t, f, bid)
	if gen != gen0+1 || mode != "bypassPermissions" || state != "invalidated" || reason != "bypass mode" {
		t.Fatalf("enter bypass: gen=%d mode=%q state=%q reason=%q", gen, mode, state, reason)
	}

	// Older mode observation arriving late must not undo bypass invalidation.
	sendMode("m-stale", "default", t0+2_000)
	gen, _, _, mode, state, _, _ = routeState(t, f, bid)
	if gen != gen0+1 || mode != "bypassPermissions" || state != "invalidated" {
		t.Fatalf("stale mode undid bypass: gen=%d mode=%q state=%q", gen, mode, state)
	}

	sendMode("m3", "default", t0+4_000)
	gen, _, _, mode, state, _, _ = routeState(t, f, bid)
	if gen != gen0+2 || mode != "default" || state != "active" {
		t.Fatalf("exit bypass: gen=%d mode=%q state=%q", gen, mode, state)
	}

	// Incomplete route (no socket) cannot be reactivated by a mode change.
	sNoSock := f.newSession("claude")
	f.hook(sNoSock, start("pid-nosock", "startup", t0+5_000))
	bid2 := bindingIDOf(t, f, f.principalOf(sNoSock))
	r := f.exec(f.caller(sNoSock), Request{
		Command:    claudeRoute("pid-nosock", "m-nosock", "", "default", t0+6_000, 1),
		Vendor:     sNoSock.vendor,
		SessionKey: sNoSock.key,
		OpKey:      "hook:claude:pid-nosock:m-nosock",
	})
	wantOK(t, r)
	_, _, _, _, state2, reason2, _ := routeState(t, f, bid2)
	if state2 != "invalidated" || reason2 != "no socket" {
		t.Fatalf("incomplete route became state=%q reason=%q, want invalidated/no socket", state2, reason2)
	}
}

func TestPublishRouteLateSessionEndAndTwoSessionsOfOneMember(t *testing.T) {
	f, _ := adapterChannel(t)
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()
	enrollAndRoute(t, f, s, "pid-1", "startup", "/tmp/cc-socks/1.sock", "default", t0, 1)
	bid := bindingIDOf(t, f, f.principalOf(s))

	// Newer session for the same member supersedes pid-1.
	enrollAndRoute(t, f, s, "pid-2", "startup", "/tmp/cc-socks/2.sock", "default", t0+2_000, 1)
	gen, _, launch, _, state, _, _ := routeState(t, f, bid)
	if launch != "pid-2" || state != "active" {
		t.Fatalf("second session: gen=%d launch=%q state=%q", gen, launch, state)
	}

	// Late SessionEnd from pid-1 must not invalidate pid-2.
	f.hook(s, ended("pid-1", t0+3_000))
	genAfter, _, launch, _, state, _, _ := routeState(t, f, bid)
	if genAfter != gen || launch != "pid-2" || state != "active" {
		t.Fatalf("after late SessionEnd: gen=%d launch=%q state=%q", genAfter, launch, state)
	}

	// Older start arriving late must not displace pid-2.
	enrollAndRoute(t, f, s, "pid-0", "startup", "/tmp/cc-socks/0.sock", "default", t0+1_000, 1)
	genAfterOld, _, launch, _, state, _, _ := routeState(t, f, bid)
	if genAfterOld != gen || launch != "pid-2" || state != "active" {
		t.Fatalf("after older start: gen=%d launch=%q state=%q", genAfterOld, launch, state)
	}
}

func TestPublishRouteStaleEpochInvalidatesAndBlocks(t *testing.T) {
	f, _ := adapterChannel(t)
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()
	enrollAndRoute(t, f, s, "pid-1", "startup", "/tmp/cc-socks/1.sock", "default", t0, 1)
	bid := bindingIDOf(t, f, f.principalOf(s))
	gen0, _, _, _, _, _, _ := routeState(t, f, bid)

	rem := true
	if r := f.owner(AdapterSet{AdapterPolicyArgs: wire.AdapterPolicyArgs{Adapter: "claude", Reminder: &rem}}, "adapter-set-1"); r.Status != core.ExitOK {
		t.Fatalf("adapter set: %+v", r)
	}
	gen, _, _, _, state, reason, _ := routeState(t, f, bid)
	if gen != gen0+1 || state != "invalidated" || reason != "epoch changed" {
		t.Fatalf("after epoch bump: gen=%d state=%q reason=%q", gen, state, reason)
	}

	// Stale epoch route registration is ignored.
	r := f.exec(f.caller(s), Request{
		Command:    claudeRoute("pid-1", "stale-ep", "/tmp/cc-socks/1.sock", "default", t0+5_000, 1),
		Vendor:     s.vendor,
		SessionKey: s.key,
		OpKey:      "hook:claude:pid-1:stale-ep",
	})
	wantOK(t, r)
	genAfter, _, _, _, state, _, _ := routeState(t, f, bid)
	if genAfter != gen || state != "invalidated" {
		t.Fatalf("stale epoch changed route: gen=%d state=%q", genAfter, state)
	}
}

func TestPublishRouteFencesSecondMemberLeafAndBadSocketPathWithoutDroppingEnroll(t *testing.T) {
	f, ch := adapterChannel(t)
	t0 := f.now.UnixMilli()
	s1 := f.newSession("claude")
	enrollAndRoute(t, f, s1, "pid-1", "startup", "/tmp/cc-socks/1.sock", "default", t0, 1)
	bid1 := bindingIDOf(t, f, f.principalOf(s1))
	gen1, _, _, _, _, _, _ := routeState(t, f, bid1)

	// A second member's leaf cannot register/overwrite s1's binding or spoof s1's session key.
	s2 := f.newSession("claude")
	f.hook(s2, start("pid-2", "startup", t0+1_000))
	c2 := f.caller(s2)
	c2.BindingID = &bid1
	r := f.exec(c2, Request{
		Command:    claudeRoute("pid-1", "spoof", "/tmp/cc-socks/99.sock", "default", t0+2_000, 1),
		Vendor:     s2.vendor,
		SessionKey: s1.key,
		OpKey:      "hook:claude:pid-1:spoof",
	})
	if r.Status == core.ExitOK {
		// Verify s1's route was not touched.
		g, _, _, _, _, _, _ := routeState(t, f, bid1)
		if g != gen1 {
			t.Fatalf("second member modified s1's route: gen %d -> %d", gen1, g)
		}
	}

	// Bad socket paths (including wrong UID and wrong temp dir) are refused on hook.route
	// while base enrollment remains live.
	s3 := f.newSession("claude")
	f.hook(s3, start("pid-3", "startup", t0+3_000))
	badPaths := []string{
		"/etc/passwd",
		"/tmp/other/123.sock",
		"/var/tmp/cc-socks/123.sock",
		"/Users/foo/cc-socks/123.sock",
		fmt.Sprintf("/tmp/cc-socks-%d/123.sock", os.Getuid()+1),
		"relative/cc-socks/123.sock",
		"/tmp/cc-socks/../evil.sock",
		"/tmp/cc-socks/not-a-socket",
	}
	for _, bad := range badPaths {
		r := f.exec(f.caller(s3), Request{
			Command:    claudeRoute("pid-3", "bad-"+bad, bad, "default", t0+4_000, 1),
			Vendor:     s3.vendor,
			SessionKey: s3.key,
			OpKey:      "hook:claude:pid-3:bad-" + bad,
		})
		if r.Status != core.ExitRefused {
			t.Fatalf("bad socket path %q: status=%d, want ExitRefused", bad, r.Status)
		}
	}
	// Base enrollment of s3 is still live!
	who := decode[WhoResult](t, f.who(ch))
	var foundS3 bool
	for _, m := range who.Members {
		if m.Alias == "claude3" {
			foundS3 = true
		}
	}
	if !foundS3 {
		t.Fatalf("bad socket path dropped base enrollment: %+v", who.Members)
	}
}

func TestPublishRouteRetiredByRestoreAndPublicRejoin(t *testing.T) {
	f, _ := adapterChannel(t)
	f.svc.RestoreKey = func(context.Context, store.Tx, int64, string, string, time.Time) (bool, error) {
		return true, nil
	}
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()
	enrollAndRoute(t, f, s, "pid-1", "startup", "/tmp/cc-socks/1.sock", "default", t0, 1)
	bid := bindingIDOf(t, f, f.principalOf(s))

	// Public leave + join (rejoin) retires the old route and bumps binding route_epoch.
	if r := f.run(s, Leave{}, "leave-1"); r.Status != core.ExitOK {
		t.Fatalf("leave: %+v", r)
	}
	f.now = f.now.Add(time.Second)
	if r := f.run(s, Join{Channel: "checkout", Alias: "claude1"}, "rejoin-1"); r.Status != core.ExitOK {
		t.Fatalf("rejoin: %+v", r)
	}
	gen, _, _, _, state, reason, _ := routeState(t, f, bid)
	if state != "ended" || reason != "left channel" {
		t.Fatalf("after rejoin: gen=%d state=%q reason=%q", gen, state, reason)
	}

	// Stale pre-leave route packet arriving after rejoin must not reactivate the route.
	rStale := f.exec(f.caller(s), Request{
		Command:    claudeRoute("pid-1", "stale-pre-leave", "/tmp/cc-socks/1.sock", "default", t0+500, 1),
		Vendor:     s.vendor,
		SessionKey: s.key,
		OpKey:      "hook:route:claude:pid-1:stale-pre-leave",
	})
	if rStale.Status != core.ExitRefused {
		t.Fatalf("stale pre-leave route after rejoin: status=%d, want ExitRefused", rStale.Status)
	}
	genAfterStale, _, _, _, stateAfterStale, _, _ := routeState(t, f, bid)
	if genAfterStale != gen || stateAfterStale != "ended" {
		t.Fatalf("stale pre-leave packet mutated route after rejoin: gen=%d state=%q", genAfterStale, stateAfterStale)
	}

	// First delivery of a pre-leave base SessionStart + route after rejoin is fenced.
	sPre := f.newSession("claude")
	f.join(sPre, Join{Channel: "checkout", Alias: "claude-rejoin"}, "join-pre")
	if r := f.run(sPre, Leave{}, "leave-pre"); r.Status != core.ExitOK {
		t.Fatalf("leave: %+v", r)
	}
	f.now = f.now.Add(2 * time.Second)
	postJoinMS := f.now.UnixMilli()
	if r := f.run(sPre, Join{Channel: "checkout", Alias: "claude-rejoin"}, "rejoin-pre"); r.Status != core.ExitOK {
		t.Fatalf("rejoin: %+v", r)
	}
	bidPre := bindingIDOf(t, f, f.principalOf(sPre))
	f.hook(sPre, start("pid-old", "startup", postJoinMS-1_000))
	if n := f.count(`SELECT COUNT(*) FROM publish_route WHERE binding_id = ?`, bidPre); n != 0 {
		t.Fatalf("pre-leave SessionStart created publish_route after rejoin: count=%d", n)
	}
	rPreRoute := f.exec(f.caller(sPre), Request{
		Command:    claudeRoute("pid-old", "pre-route", "/tmp/cc-socks/1.sock", "default", postJoinMS-1_000, 1),
		Vendor:     sPre.vendor,
		SessionKey: sPre.key,
		OpKey:      "hook:route:claude:pid-old:pre-route",
	})
	if rPreRoute.Status != core.ExitRefused {
		t.Fatalf("pre-leave hook.route after rejoin: status=%d, want ExitRefused", rPreRoute.Status)
	}

	// Fresh post-rejoin SessionStart + route is accepted.
	enrollAndRoute(t, f, sPre, "pid-new", "startup", "/tmp/cc-socks/2.sock", "default", postJoinMS+1_000, 1)
	_, _, launchPre, _, statePre, _, _ := routeState(t, f, bidPre)
	if launchPre != "pid-new" || statePre != "active" {
		t.Fatalf("fresh post-rejoin start: launch=%q state=%q", launchPre, statePre)
	}

	// Re-register route, then public owner restore retires it.
	enrollAndRoute(t, f, s, "pid-2", "startup", "/tmp/cc-socks/2.sock", "default", t0+5_000, 1)
	if r := f.owner(Restore{BindingID: bid, Vendor: "claude", Key: "restored-session"}, "restore-1"); r.Status != core.ExitOK {
		t.Fatalf("restore: %+v", r)
	}
	_, _, _, _, state, reason, _ = routeState(t, f, bid)
	if state != "ended" || reason != "binding restored" {
		t.Fatalf("after restore: state=%q reason=%q", state, reason)
	}
}

func TestPublishRouteWhoShowsCandidateOnly(t *testing.T) {
	f, peer := adapterChannel(t)
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()
	enrollAndRoute(t, f, s, "pid-1", "startup", "/tmp/cc-socks/1.sock", "default", t0, 1)

	who := decode[WhoResult](t, f.who(peer))
	var sawClaude, sawPeer bool
	for _, m := range who.Members {
		if m.Vendor == "claude" {
			sawClaude = true
			if m.Publish == nil || m.Publish.State != "candidate" {
				t.Fatalf("claude member publish = %+v, want candidate", m.Publish)
			}
		}
		if m.Alias == "writer" {
			sawPeer = true
			if m.Publish != nil {
				t.Fatalf("writer member publish = %+v, want nil (no route)", m.Publish)
			}
		}
	}
	if !sawClaude || !sawPeer {
		t.Fatalf("missing members in who: %+v", who.Members)
	}
}

func TestPublishRouteSocketSupplementDeliveredLast(t *testing.T) {
	f, _ := adapterChannel(t)
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()

	// 1. Base enroll arrives, then newer bypassPermissions mode arrives BEFORE socket supplement.
	f.hook(s, start("pid-1", "startup", t0))
	bid := bindingIDOf(t, f, f.principalOf(s))
	wantOK(t, f.exec(f.caller(s), Request{
		Command:    claudeRoute("pid-1", "m-bypass", "", "bypassPermissions", t0+1_000, 1),
		Vendor:     s.vendor,
		SessionKey: s.key,
		OpKey:      "hook:route:claude:pid-1:m-bypass",
	}))
	// Now the SessionStart socket supplement (at t0, mode "default") arrives last.
	wantOK(t, f.exec(f.caller(s), Request{
		Command:    claudeRoute("pid-1", "start-sock", "/tmp/cc-socks/1.sock", "default", t0, 1),
		Vendor:     s.vendor,
		SessionKey: s.key,
		OpKey:      "hook:route:claude:pid-1:start-sock",
	}))
	_, _, _, mode, state, reason, _ := routeState(t, f, bid)
	if mode != "bypassPermissions" || state != "invalidated" || reason != "bypass mode" {
		t.Fatalf("supplement overwrote newer bypass: mode=%q state=%q reason=%q", mode, state, reason)
	}

	// 2. Enroll + SessionEnd, then socket supplement arrives last: must keep ended state.
	s2 := f.newSession("claude")
	f.hook(s2, start("pid-2", "startup", t0+2_000))
	bid2 := bindingIDOf(t, f, f.principalOf(s2))
	f.hook(s2, ended("pid-2", t0+3_000))
	rEnded := f.exec(f.caller(s2), Request{
		Command:    claudeRoute("pid-2", "late-sock", "/tmp/cc-socks/2.sock", "default", t0+2_000, 1),
		Vendor:     s2.vendor,
		SessionKey: s2.key,
		OpKey:      "hook:route:claude:pid-2:late-sock",
	})
	if rEnded.Status != core.ExitRefused {
		t.Fatalf("late supplement after SessionEnd: status=%d, want ExitRefused", rEnded.Status)
	}
	_, _, _, _, state2, reason2, _ := routeState(t, f, bid2)
	if state2 != "ended" || reason2 != "session ended" {
		t.Fatalf("supplement reactivated ended route: state=%q reason=%q", state2, reason2)
	}
}
