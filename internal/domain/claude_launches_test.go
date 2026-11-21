package domain

import (
	"context"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
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
	if applyClaudeProbes(f, ClaudeLaunchProbe{ClaudeLaunch: unknown, PID: 1, StartIdentity: 20}) {
		t.Fatal("a probe associated the launch with another PID")
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

func TestClaudeProcessExitAppliesUnboundProofWithoutRetiringQueue(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	p.turnEndAsk(p.writer, "reader", "unbound-exit")
	id := p.reserve()
	if id == 0 {
		t.Fatal("fixture did not reserve a notice")
	}
	p.mutate(`UPDATE adapter_launch SET process_pid = NULL, process_start_identity = NULL WHERE launch_ref = ?`, publishTestLaunch)
	stale := liveClaudeLaunches(p.fixture, 0, 1)[0]
	p.mutate(`UPDATE adapter_launch SET pid_namespace = 'pid:[200]' WHERE launch_ref = ?`, publishTestLaunch)
	if applyClaudeProbes(p.fixture, ClaudeLaunchProbe{ClaudeLaunch: stale, Gone: true}) {
		t.Fatal("a legacy snapshot crossed the namespace fence")
	}
	launch := liveClaudeLaunches(p.fixture, 0, 1)[0]
	if launch.PIDNamespace != "pid:[200]" || launch.ProcessPID != 0 || launch.ProcessStartIdentity != 0 {
		t.Fatalf("wrong unbound launch snapshot: %+v", launch)
	}
	if !applyClaudeProbes(p.fixture, ClaudeLaunchProbe{ClaudeLaunch: launch, Gone: true}) {
		t.Fatal("a current unbound process exit proof was not applied")
	}
	if _, _, _, _, state, _, _ := routeState(t, p.fixture, launch.BindingID); state != "ended" {
		t.Fatalf("the unbound launch kept its publish route %q", state)
	}
	if n := p.count(`SELECT COUNT(*) FROM binding WHERE id = ? AND state = 'departed' AND departed_by_hook = 1`, launch.BindingID); n != 1 {
		t.Fatal("last unbound launch did not depart by hook")
	}
	p.state(id, "reserved")
	if n := p.count(`SELECT COUNT(*) FROM publish_route_proof WHERE binding_id = ?`, launch.BindingID); n != 1 {
		t.Fatal("the unbound exit retired a physical publish queue")
	}
}

func TestClaudePreviousBootRetiresOnlyItsPublishLane(t *testing.T) {
	for _, outcome := range []PublishOutcome{PublishNotSubmitted, PublishSubmitted} {
		t.Run(string(outcome), func(t *testing.T) {
			p := newPublishFixture(t)
			p.proveRoute()
			p.turnEndAsk(p.writer, "reader", "old-boot")
			attempt := p.reserve()
			if attempt == 0 {
				t.Fatal("fixture did not reserve the old notice")
			}
			if outcome == PublishSubmitted {
				if ids := p.begin(attempt); len(ids) != 1 {
					t.Fatalf("began %v, want the old notice", ids)
				}
				p.resolve(attempt, outcome)
			} else {
				p.mutate(`UPDATE adapter_launch SET process_pid = NULL, process_start_identity = NULL WHERE launch_ref = ?`, publishTestLaunch)
			}
			p.mutate(`UPDATE adapter_launch SET pid_namespace = '12345678-1234-1234-1234-123456789abc:100' WHERE launch_ref = ?`, publishTestLaunch)
			old := liveClaudeLaunches(p.fixture, 0, 1)[0]

			peer := p.newSession("claude")
			wantOK(t, p.join(peer, Join{Channel: "checkout", Alias: "peer"}, "peer-join"))
			p.now = p.now.Add(time.Second)
			enrollAndRoute(t, p.fixture, peer, "pid-42-current", "startup", publishTestSocket, "default", p.now.UnixMilli(), 1)
			p.mutate(`UPDATE adapter_launch SET pid_namespace = '12345678-1234-1234-1234-123456789def:100' WHERE launch_ref = 'pid-42-current'`)
			peerID := bindingIDOf(t, p.fixture, p.principalOf(peer))
			gen, _, _, _, _, _, _ := routeState(t, p.fixture, peerID)
			p.update(func(ctx context.Context, tx store.Tx) error {
				return ConfirmPublishRoute(ctx, tx, PublishBinding{ID: peerID, Generation: gen}, p.queue)
			})
			if n := p.count(`SELECT COUNT(*) FROM publish_route_proof WHERE process_pid = ? AND process_start_ms = ?`, p.queue.PID, p.queue.StartMS); n != 2 {
				t.Fatalf("fixture proved %d queues, want two identical numeric identities", n)
			}

			probe := ClaudeLaunchProbe{ClaudeLaunch: old, Gone: true, PreviousBoot: true}
			if !applyClaudeProbes(p.fixture, probe) || applyClaudeProbes(p.fixture, probe) {
				t.Fatal("prior-boot exit did not commit exactly once")
			}
			p.state(attempt, string(outcome))
			if n := p.count(`SELECT COUNT(*) FROM publish_attempt WHERE id = ? AND `+openPublishLane, attempt); n != 0 {
				t.Fatal("the old attempt still held the queue")
			}
			if n := p.count(`SELECT COUNT(*) FROM publish_route_proof WHERE binding_id = ?`, old.BindingID); n != 0 {
				t.Fatal("prior-boot route kept its proof")
			}
			if n := p.count(`SELECT COUNT(*) FROM publish_route_proof WHERE binding_id = ? AND generation = ? AND process_pid = ? AND process_start_ms = ?`,
				peerID, gen, p.queue.PID, p.queue.StartMS); n != 1 {
				t.Fatal("prior-boot exit removed the current queue's proof")
			}
		})
	}
}

func TestClaudeLaunchPinsItsFirstKnownPIDNamespace(t *testing.T) {
	f, _ := adapterChannel(t)
	s := f.newSession("claude")
	at := f.now.UnixMilli()
	cmd := start("pid-7-a", "startup", at)
	cmd.PIDNamespace = "pid:[100]"
	f.hook(s, cmd)
	launch := liveClaudeLaunches(f, 0, 1)[0]
	if launch.PIDNamespace != cmd.PIDNamespace || launch.StartedAtMS != at {
		t.Fatalf("first namespace was not stored: %+v", launch)
	}
	conflict := start(launch.LaunchRef, "resume", at+10)
	conflict.PIDNamespace = "pid:[200]"
	if r := f.hook(s, conflict); !quiet(r) || liveClaudeLaunches(f, 0, 1)[0] != launch {
		t.Fatalf("conflicting namespace changed the existing launch: %+v", r)
	}
	f.hook(s, start(launch.LaunchRef, "resume", at+20))
	if got := liveClaudeLaunches(f, 0, 1)[0]; got.PIDNamespace != cmd.PIDNamespace {
		t.Fatalf("legacy resume cleared the verified namespace: %+v", got)
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
