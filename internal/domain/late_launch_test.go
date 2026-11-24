package domain

import (
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// #252: a session whose start never reached the dispatcher has its process
// launch bound by a prompt, under the checks of a hand join.

func lateFixture(t *testing.T) (*fixture, session, map[string]bool) {
	t.Helper()
	f, writer := adapterChannel(t)
	alive := map[string]bool{"pid-7": true, "pid-8": true, "pid-9": true}
	f.svc.LaunchProcess = testProcess
	f.svc.LaunchAlive = func(ref string, _ int64) bool {
		process, _ := testProcess(ref)
		return alive[process]
	}
	return f, writer, alive
}

// hookAs runs a hook verb with the session key, as the spool resolver does,
// so a route can name its session.
func (f *fixture) hookAs(s session, cmd Command) wire.HookResult {
	f.t.Helper()
	var at int64
	switch c := cmd.(type) {
	case HookEnroll:
		at = c.HookAtMS
	case HookEvent:
		at = c.HookAtMS
	}
	if at > f.now.UnixMilli() && at <= f.now.UnixMilli()+30_000 {
		f.now = time.UnixMilli(at)
	}
	caller := f.caller(s)
	req := Request{Command: cmd, Vendor: s.vendor, SessionKey: s.key, OpKey: hookKey(cmd)}
	if caller.Role == core.RoleUnenrolled {
		req.Enroll = enroll(s)
	}
	r := f.exec(caller, req)
	wantOK(f.t, r)
	return decode[wire.HookResult](f.t, r)
}

// lateProof adds what a hook sends with a prompt of a process launch: the
// time its launch record was written.
func lateProof(cmd HookEvent, launchAt int64) HookEvent {
	cmd.LaunchAtMS = launchAt
	return cmd
}

type launchRow struct {
	ref      string
	verified bool
	started  int64
	mode     string
}

func (f *fixture) launches(s session) []launchRow {
	f.t.Helper()
	rows, err := f.db.Query(`SELECT launch_ref, verified, started_hook_at_ms, json_extract(profile, '$.mode')
		FROM adapter_launch WHERE principal_id = ? ORDER BY rowid`, *f.caller(s).PrincipalID)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []launchRow
	for rows.Next() {
		var l launchRow
		if err := rows.Scan(&l.ref, &l.verified, &l.started, &l.mode); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, l)
	}
	return out
}

func (f *fixture) routeLaunch(s session) string {
	f.t.Helper()
	var launch string
	if err := f.db.QueryRow(`SELECT COALESCE(MAX(r.launch_ref), '') FROM publish_route r JOIN binding b ON b.id = r.binding_id
		WHERE b.principal_id = ?`, *f.caller(s).PrincipalID).Scan(&launch); err != nil {
		f.t.Fatal(err)
	}
	return launch
}

func (f *fixture) lateBinds() int {
	return f.count(`SELECT COUNT(*) FROM event WHERE kind = 'launch_bound_late'`)
}

func TestARecoveringPromptBindsItsProcessLaunch(t *testing.T) {
	f, _, _ := lateFixture(t)
	s := f.newSession("claude")
	at := f.now.UnixMilli()
	r := f.hookAs(s, lateProof(recoveryPrompt("pid-7-a", "t1", at+10), at))
	if r.Enrollment != "joined" {
		t.Fatalf("recovering prompt: %+v, want joined", r)
	}
	if got := f.launches(s); len(got) != 1 || got[0] != (launchRow{"pid-7-a", true, at, "print"}) {
		t.Fatalf("launches %+v, want the process launch, verified, from its record time, in the prompt's mode", got)
	}
	if got := f.routeLaunch(s); got != "pid-7-a" {
		t.Fatalf("route launch %q, want pid-7-a", got)
	}
	if n := f.lateBinds(); n != 1 {
		t.Fatalf("%d launch_bound_late events, want 1", n)
	}
	// The prompt itself and the next one belong to the launch: no
	// "recovered" rewrite to the session launch.
	f.hookAs(s, lateProof(prompt("pid-7-a", "t2", at+20), at))
	if n := f.count(`SELECT COUNT(*) FROM observation WHERE principal_id = ? AND launch_ref = 'pid-7-a' AND kind = 'prompt_submitted'`,
		*f.caller(s).PrincipalID); n != 2 {
		t.Fatalf("%d prompts recorded on the process launch, want 2", n)
	}
	if n := f.lateBinds(); n != 1 {
		t.Fatalf("a prompt of a bound launch bound it again: %d events", n)
	}
	// The start that lost its request, arriving now, is a replay.
	if got := f.hookAs(s, start("pid-7-a", "startup", at)); !quiet(got) {
		t.Fatalf("the delayed start printed %+v", got)
	}
	if got := f.launches(s); len(got) != 1 || got[0].started != at {
		t.Fatalf("the delayed start changed the launches: %+v", got)
	}
}

// Each refusal leaves exactly today's rows: a session-scoped launch from
// time 0, no route, no event.
func TestARecoveringPromptKeepsTheSessionLaunchWhenTheBindIsRefused(t *testing.T) {
	cases := []struct {
		name   string
		before func(*fixture, map[string]bool)
		cmd    func(at int64) HookEvent
	}{
		{"no launch time", nil, func(at int64) HookEvent { return recoveryPrompt("pid-7-a", "t1", at+10) }},
		{"an unverified launch", nil, func(at int64) HookEvent {
			cmd := lateProof(recoveryPrompt("pid-7-a", "t1", at+10), at)
			cmd.LaunchVerified = false
			return cmd
		}},
		{"a launch time after the prompt", nil, func(at int64) HookEvent {
			return lateProof(recoveryPrompt("pid-7-a", "t1", at+10), at+11)
		}},
		{"the process is gone", func(_ *fixture, alive map[string]bool) { alive["pid-7"] = false }, func(at int64) HookEvent {
			return lateProof(recoveryPrompt("pid-7-a", "t1", at+10), at)
		}},
		{"another PID namespace", func(f *fixture, _ map[string]bool) {
			f.svc.LaunchPIDNamespace = func() string { return "dispatcher-namespace" }
		}, func(at int64) HookEvent {
			cmd := lateProof(recoveryPrompt("pid-7-a", "t1", at+10), at)
			cmd.PIDNamespace = "other-namespace"
			return cmd
		}},
		{"a present member holds the process", func(f *fixture, _ map[string]bool) {
			f.hook(f.newSession("claude"), start("pid-7-x", "startup", f.now.UnixMilli()))
		}, func(at int64) HookEvent { return lateProof(recoveryPrompt("pid-7-a", "t1", at+10), at) }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f, _, alive := lateFixture(t)
			if c.before != nil {
				c.before(f, alive)
			}
			s := f.newSession("claude")
			if r := f.hookAs(s, c.cmd(f.now.UnixMilli())); r.Enrollment != "joined" {
				t.Fatalf("recovering prompt: %+v, want joined as today", r)
			}
			if got := f.launches(s); len(got) != 1 || got[0] != (launchRow{SessionLaunch, false, 0, "unverified"}) {
				t.Fatalf("launches %+v, want today's session launch", got)
			}
			if got := f.routeLaunch(s); got != "" {
				t.Fatalf("route on %q, want none", got)
			}
			if n := f.lateBinds(); n != 0 {
				t.Fatalf("%d launch_bound_late events", n)
			}
		})
	}
}

// A member recovered before this change (session launch from time 0) is
// repaired by its next prompt.
func TestARecoveredMemberIsBoundByItsNextPrompt(t *testing.T) {
	f, _, _ := lateFixture(t)
	s := f.newSession("claude")
	at := f.now.UnixMilli()
	f.hookAs(s, recoveryPrompt("pid-7-a", "t1", at+10))
	f.hookAs(s, lateProof(prompt("pid-7-a", "t2", at+20), at))
	got := f.launches(s)
	if len(got) != 2 || got[1] != (launchRow{"pid-7-a", true, at, "print"}) {
		t.Fatalf("launches %+v, want the process launch bound beside the old session launch", got)
	}
	if f.routeLaunch(s) != "pid-7-a" || f.lateBinds() != 1 {
		t.Fatalf("route %q, %d events", f.routeLaunch(s), f.lateBinds())
	}
	f.hookAs(s, lateProof(stop("pid-7-a", "t2", at+30), at))
	if n := f.count(`SELECT COUNT(*) FROM observation WHERE launch_ref = 'pid-7-a' AND kind = 'turn_finishing'`); n != 1 {
		t.Fatal("a later event of the bound launch was rewritten to the session launch")
	}
}

// A known member whose resume start was missed: its prompts used to be
// stored only. The prompt rejoins it under its new launch.
func TestAMemberWhoseResumeStartWasMissedRejoins(t *testing.T) {
	f, writer, _ := lateFixture(t)
	s := f.newSession("claude")
	at := f.now.UnixMilli()
	f.hookAs(s, start("pid-7-a", "startup", at))
	f.hookAs(s, ended("pid-7-a", at+10))
	live := func() int { return f.count(`SELECT COUNT(*) FROM binding WHERE alias = 'claude1' AND state = 'live'`) }
	if live() != 0 {
		t.Fatal("the clean end did not depart the member")
	}
	f.hookAs(s, lateProof(prompt("pid-9-b", "t1", at+30), at+20))
	if live() != 1 || f.memberRow(writer, "claude1").Alias != "claude1" {
		t.Fatal("the missed resume's prompt did not bring the member back")
	}
	if got := f.launches(s); len(got) != 2 || got[1] != (launchRow{"pid-9-b", true, at + 20, "print"}) {
		t.Fatalf("launches %+v", got)
	}
	if f.routeLaunch(s) != "pid-9-b" || f.lateBinds() != 1 {
		t.Fatalf("route %q, %d events", f.routeLaunch(s), f.lateBinds())
	}
}

func TestAKnownMemberIsNotBoundLateWhereAStartWouldNotRestoreIt(t *testing.T) {
	latePrompt := func(at int64) HookEvent { return lateProof(prompt("pid-9-b", "t1", at+30), at+20) }
	for _, c := range []struct {
		name   string
		absent func(*fixture, session, int64)
		cmd    func(at int64) HookEvent
	}{
		{"a Stop", func(*fixture, session, int64) {}, func(at int64) HookEvent {
			return lateProof(stop("pid-9-b", "t1", at+30), at+20)
		}},
		{"a prompt ahead of the dispatcher's clock", func(*fixture, session, int64) {}, func(at int64) HookEvent {
			return lateProof(prompt("pid-9-b", "t1", at+60_000), at+20)
		}},
		{"left by hand", func(f *fixture, s session, _ int64) { wantOK(f.t, f.run(s, Leave{}, "")) }, latePrompt},
		{"behind the owner's fence", func(f *fixture, s session, at int64) {
			if _, err := f.db.Exec(`UPDATE binding SET owner_hook_fence_ms = ? WHERE principal_id = ?`,
				at+20, *f.caller(s).PrincipalID); err != nil {
				f.t.Fatal(err)
			}
		}, latePrompt},
		{"a prompt captured before a move", func(f *fixture, s session, at int64) {
			if _, err := f.db.Exec(`UPDATE member_selection SET generation = 2, selected_at_ms = ? WHERE principal_id = ?`,
				at+30, *f.caller(s).PrincipalID); err != nil {
				f.t.Fatal(err)
			}
		}, latePrompt},
		{"a launch time before the clean end", func(f *fixture, s session, at int64) {
			f.hookAs(s, ended("pid-7-a", at+25))
		}, latePrompt},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, _, _ := lateFixture(t)
			s := f.newSession("claude")
			at := f.now.UnixMilli()
			f.hookAs(s, start("pid-7-a", "startup", at))
			c.absent(f, s, at)
			f.hookAs(s, c.cmd(at))
			if got := f.launches(s); len(got) != 1 {
				t.Fatalf("launches %+v, want only the started one", got)
			}
			if f.lateBinds() != 0 {
				t.Fatal("launch_bound_late written")
			}
		})
	}
}

// A member rejoined by hand, whose new launch the join did not bind, gets
// its route from the prompt that binds the launch, though the launch was
// recorded before the rejoin.
func TestALaunchBoundLateAfterAHandRejoinGetsItsRoute(t *testing.T) {
	f, _, _ := lateFixture(t)
	s := f.newSession("claude")
	at := f.now.UnixMilli()
	f.hookAs(s, start("pid-7-a", "startup", at))
	wantOK(t, f.run(s, Leave{}, ""))
	f.now = time.UnixMilli(at + 40)
	wantOK(t, f.join(s, Join{Channel: "checkout", Alias: "claude1"}, ""))
	f.hookAs(s, lateProof(prompt("pid-9-b", "t1", at+60), at+20))
	if got := f.launches(s); len(got) != 2 || got[1] != (launchRow{"pid-9-b", true, at + 20, "print"}) {
		t.Fatalf("launches %+v", got)
	}
	var launch, state string
	var epoch int64
	if err := f.db.QueryRow(`SELECT r.launch_ref, r.state || ":" || r.reason, r.binding_epoch FROM publish_route r JOIN binding b ON b.id = r.binding_id
		WHERE b.principal_id = ?`, *f.caller(s).PrincipalID).Scan(&launch, &state, &epoch); err != nil {
		t.Fatal(err)
	}
	// The fixture has no socket, so a registered route is invalidated for
	// that alone; the rejoin left the old one ended.
	if launch != "pid-9-b" || state != "invalidated:no socket" || epoch != 2 {
		t.Fatalf("route %s %s at epoch %d, want pid-9-b registered at epoch 2", launch, state, epoch)
	}
}

func (f *fixture) startUnseen(alias string) bool {
	f.t.Helper()
	result := f.owner(OwnerWho{}, "")
	wantOK(f.t, result)
	for _, channel := range decode[OwnerWhoResult](f.t, result).Channels {
		for _, member := range channel.Members {
			if member.Alias == alias {
				return member.StartUnseen
			}
		}
	}
	f.t.Fatalf("missing @%s", alias)
	return false
}

// The owner sees why a member the bind refused has no route; the member
// is told nothing new.
func TestTheOwnerSeesWhyARecoveredMemberHasNoRoute(t *testing.T) {
	f, _, alive := lateFixture(t)
	alive["pid-7"] = false
	s := f.newSession("claude")
	at := f.now.UnixMilli()
	f.hookAs(s, lateProof(recoveryPrompt("pid-7-a", "t1", at+10), at))
	if !f.startUnseen("claude1") {
		t.Fatal("owner who does not say the refused member's start was not seen")
	}
	alive["pid-7"] = true
	f.hookAs(s, lateProof(prompt("pid-7-a", "t2", at+20), at))
	if f.startUnseen("claude1") {
		t.Fatal("owner who still says the start was not seen after the late bind")
	}
	started := f.newSession("claude")
	f.hookAs(started, start("pid-8-a", "startup", at+30))
	if f.startUnseen("claude2") {
		t.Fatal("a started member is reported without a seen start")
	}
}
