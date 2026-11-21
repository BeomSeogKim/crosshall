package domain

import (
	"testing"
	"time"
)

func liveClaudeLaunches(f *fixture, after int64, limit int) []ClaudeLaunch {
	f.t.Helper()
	launches, err := LiveClaudeLaunches(f.t.Context(), f.db, after, limit)
	if err != nil {
		f.t.Fatal(err)
	}
	return launches
}

func applyClaudeProbes(f *fixture, results ...ClaudeLaunchProbe) bool {
	f.t.Helper()
	ctx := f.t.Context()
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		f.t.Fatal(err)
	}
	defer tx.Rollback()
	changed, err := ApplyClaudeLaunchProbes(ctx, tx, results, f.now)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		f.t.Fatal(err)
	}
	return changed
}

func TestClaudeProcessExitEndsPublishAndAllowsDelayedResume(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	ask := p.turnEndAsk(p.writer, "reader", "process-exit")
	attempt := p.reserve()
	if attempt == 0 {
		t.Fatal("fixture did not reserve the notice")
	}
	launches := liveClaudeLaunches(p.fixture, 0, 5)
	if len(launches) != 1 || launches[0].ProcessPID != p.queue.PID || launches[0].ProcessStartIdentity != p.queue.StartMS {
		t.Fatalf("publish proof didn't pin the launch: %+v", launches)
	}
	launch := launches[0]
	p.now = p.now.Add(2 * time.Second)
	probe := ClaudeLaunchProbe{ClaudeLaunch: launch, Gone: true}
	if !applyClaudeProbes(p.fixture, probe) || applyClaudeProbes(p.fixture, probe) {
		t.Fatal("known death did not commit exactly once")
	}
	if n := p.count(`SELECT COUNT(*) FROM adapter_launch WHERE principal_id = ? AND launch_ref = ?
		AND process_pid = ? AND process_start_identity = ? AND ended_by_process = 1 AND ended_hook_at_ms = ?`,
		launch.PrincipalID, launch.LaunchRef, p.queue.PID, p.queue.StartMS, p.now.UnixMilli()); n != 1 {
		t.Fatal("exit did not preserve the exact process identity and record an end")
	}
	if gen, _, ref, _, state, _, _ := routeState(t, p.fixture, launch.BindingID); gen != p.binding.Generation+1 || ref != launch.LaunchRef || state != "ended" {
		t.Fatalf("exit did not end the matching route: %q %q generation %d", ref, state, gen)
	}
	p.state(attempt, string(PublishNotSubmitted))
	if got := p.begin(attempt); len(got) != 0 {
		t.Fatalf("ended route can still begin publish: %v", got)
	}
	if n := p.count(`SELECT COUNT(*) FROM publish_route_proof WHERE binding_id = ?`, launch.BindingID); n != 0 {
		t.Fatal("proved-gone queue retained its route proof")
	}
	if n := p.count(`SELECT COUNT(*) FROM binding WHERE id = ? AND state = 'departed' AND departed_by_hook = 1`, launch.BindingID); n != 1 {
		t.Fatal("the last proved-gone launch did not depart by hook")
	}
	if n := p.count(`SELECT COUNT(*) FROM roster_change WHERE principal_id = ? AND field = 'departed'`, launch.PrincipalID); n != 1 {
		t.Fatalf("departure roster changes = %d, want one", n)
	}
	if n := p.count(`SELECT COUNT(*) FROM capability WHERE principal_id = ? AND available = 1`, launch.PrincipalID); n != 0 {
		t.Fatalf("dead member retained %d available capabilities", n)
	}
	if n := p.count(`SELECT COUNT(*) FROM obligation WHERE message_id = ? AND principal_id = ? AND state = 'open'`, ask, launch.PrincipalID); n != 1 || p.keyState(p.reader) != "live" {
		t.Fatal("process exit lost the member's key or obligation")
	}
	if r := p.hook(p.reader, start(launch.LaunchRef, "resume", p.now.UnixMilli()+100)); !quiet(r) {
		t.Fatalf("proved dead reference reopened: %+v", r)
	}
	if r := p.hook(p.reader, start("pid-43-new", "resume", launch.StartedAtMS+1000)); r.Enrollment != "restored" || r.Alias != "reader" {
		t.Fatalf("resume issued before death detection did not restore the same member: %+v", r)
	}
	if bindingIDOf(t, p.fixture, p.principalOf(p.reader)) != launch.BindingID {
		t.Fatal("resume replaced the old binding")
	}
}

func TestClaudeProcessExitKeepsAnUnknownOverlap(t *testing.T) {
	f, writer := adapterChannel(t)
	s := f.newSession("claude")
	at := f.now.UnixMilli()
	f.hook(s, start("pid-1-a", "startup", at))
	f.hook(s, start("pid-2-b", "resume", at+10))
	first := liveClaudeLaunches(f, 0, 1)
	if len(first) != 1 {
		t.Fatalf("first page: %+v", first)
	}
	second := liveClaudeLaunches(f, first[0].RowID, 3)
	if len(second) != 1 || second[0].LaunchRef != "pid-2-b" {
		t.Fatalf("second page: %+v", second)
	}
	unknown := second[0]
	if applyClaudeProbes(f, ClaudeLaunchProbe{ClaudeLaunch: unknown, Gone: true}) ||
		applyClaudeProbes(f, ClaudeLaunchProbe{ClaudeLaunch: unknown, PID: 1, StartIdentity: 20}) {
		t.Fatal("an unbound probe applied death or associated another PID")
	}
	if !applyClaudeProbes(f, ClaudeLaunchProbe{ClaudeLaunch: unknown, PID: 2, StartIdentity: 20}) {
		t.Fatal("a matching positive probe did not pin the identity")
	}
	bound := liveClaudeLaunches(f, first[0].RowID, 3)[0]
	if !applyClaudeProbes(f, ClaudeLaunchProbe{ClaudeLaunch: bound, Gone: true}) || f.memberRow(writer, "claude1").Alias == "" {
		t.Fatal("the unknown older launch did not keep the binding live")
	}
	if n := f.count(`SELECT COUNT(*) FROM adapter_launch WHERE launch_ref = 'pid-1-a' AND ended_hook_at_ms IS NULL`); n != 1 {
		t.Fatal("unknown older launch was terminated")
	}
}

func TestStaleClaudeProcessProbeCannotDepartClearedMember(t *testing.T) {
	f, writer, old, owed, unread := clearFixture(t, true)
	launch := liveClaudeLaunches(f, 0, 1)[0]
	if !applyClaudeProbes(f, ClaudeLaunchProbe{ClaudeLaunch: launch, PID: 7, StartIdentity: 100}) {
		t.Fatal("fixture did not pin the prior launch identity")
	}
	launch = liveClaudeLaunches(f, 0, 1)[0]
	cleared := f.newSession("claude")
	result := f.hook(cleared, start("pid-7-b", "clear", f.now.UnixMilli()+20))
	if result.Enrollment != "restored" {
		t.Fatalf("fixture clear did not restore: %+v", result)
	}
	if applyClaudeProbes(f, ClaudeLaunchProbe{ClaudeLaunch: launch, Gone: true}) {
		t.Fatal("a stale probe altered state after a clear")
	}
	wantTakenOver(t, f, writer, old, cleared, owed, unread, result.Cue+"\n"+result.PendingLine)
}

func TestStaleClaudeProcessProbeDoesNotRewriteCleanEnd(t *testing.T) {
	f, _ := adapterChannel(t)
	s := f.newSession("claude")
	f.hook(s, start("pid-7-launch", "startup", f.now.UnixMilli()))
	launch := liveClaudeLaunches(f, 0, 1)[0]
	if !applyClaudeProbes(f, ClaudeLaunchProbe{ClaudeLaunch: launch, PID: 7, StartIdentity: 100}) {
		t.Fatal("fixture did not pin the launch identity")
	}
	launch = liveClaudeLaunches(f, 0, 1)[0]
	cleanEndAt := f.now.UnixMilli() + 100
	f.hook(s, ended(launch.LaunchRef, cleanEndAt))
	f.now = f.now.Add(time.Second)
	if applyClaudeProbes(f, ClaudeLaunchProbe{ClaudeLaunch: launch, Gone: true}) {
		t.Fatal("a stale probe changed a clean end into a synthetic one")
	}
	if n := f.count(`SELECT COUNT(*) FROM adapter_launch WHERE principal_id = ? AND launch_ref = ?
		AND ended_by_process = 0 AND ended_hook_at_ms = ?`, launch.PrincipalID, launch.LaunchRef, cleanEndAt); n != 1 {
		t.Fatal("native clean end was not preserved")
	}
	if r := f.hook(s, start("pid-8-launch", "resume", cleanEndAt-1)); !quiet(r) {
		t.Fatalf("resume predating a native clean end restored a member: %+v", r)
	}
}
