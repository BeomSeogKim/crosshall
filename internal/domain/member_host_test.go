package domain

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// hostFixture is a channel with two members that joined by hand and named
// hosts: "worker" in process 100 and "lead" in process 200. gone holds the
// processes the service is told have ended.
type hostFixture struct {
	*fixture
	worker, lead session
	gone         map[int64]bool
}

func newHostFixture(t *testing.T) *hostFixture {
	f := &hostFixture{fixture: newFixture(t), gone: map[int64]bool{}}
	f.svc.ProcessGone = func(pid, identity int64) bool { return f.gone[pid] }
	wantOK(t, f.owner(CreateChannel{Name: "dev"}, "create"))
	f.worker, f.lead = f.newSession("codex"), f.newSession("route")
	wantOK(t, f.hostJoin(f.worker, "worker", &wire.Host{PID: 100, Start: 7}))
	wantOK(t, f.hostJoin(f.lead, "lead", &wire.Host{PID: 200, Start: 9}))
	return f
}

func (f *hostFixture) hostJoin(s session, alias string, h *wire.Host) core.Result {
	c := f.resolved(s)
	req := Request{OpKey: "join-" + alias, Command: Join{Channel: "dev", Alias: alias}, Vendor: s.vendor, Host: h}
	if c.PrincipalID == nil {
		req.Enroll = enroll(s)
	}
	return f.exec(c, req)
}

// command runs an ordinary member command as the dispatcher resolves it.
func (f *hostFixture) command(s session, cmd Command, key string, h *wire.Host) core.Result {
	return f.exec(f.resolved(s), Request{OpKey: key, Command: cmd, Vendor: s.vendor, Host: h})
}

func (f *hostFixture) principal(s session) int64 { return *f.caller(s).PrincipalID }

func (f *hostFixture) host(s session) (pid, start int64, seen int64, ok bool) {
	err := f.db.QueryRow(`SELECT pid, start_identity, seen_at_ms FROM member_host WHERE principal_id = ?`, f.principal(s)).
		Scan(&pid, &start, &seen)
	return pid, start, seen, err == nil
}

func (f *hostFixture) bindingState(s session) (state string, byHook bool, alias string) {
	f.t.Helper()
	if err := f.db.QueryRow(`SELECT state, departed_by_hook, alias FROM binding WHERE principal_id = ?`, f.principal(s)).
		Scan(&state, &byHook, &alias); err != nil {
		f.t.Fatal(err)
	}
	return
}

// sweep lists the tracked members and departs those whose host is gone, as
// the dispatcher's sweep does.
func (f *hostFixture) sweep() {
	f.t.Helper()
	tx, err := f.db.Begin()
	if err != nil {
		f.t.Fatal(err)
	}
	defer tx.Rollback()
	members, err := HostedMembers(f.t.Context(), tx, 0, 32)
	if err != nil {
		f.t.Fatal(err)
	}
	var gone []HostedMember
	for _, m := range members {
		if f.gone[m.PID] {
			gone = append(gone, m)
		}
	}
	if _, err := DepartGoneHosts(f.t.Context(), tx, gone, f.now); err != nil {
		f.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		f.t.Fatal(err)
	}
}

func TestMemberHostIsStoredReplacedAndKept(t *testing.T) {
	f := newHostFixture(t)
	if pid, start, _, ok := f.host(f.worker); !ok || pid != 100 || start != 7 {
		t.Fatalf("join stored %d %d %v", pid, start, ok)
	}
	_, _, seen, _ := f.host(f.worker)
	f.now = f.now.Add(time.Minute)
	wantOK(t, f.command(f.worker, Who{}, "", &wire.Host{PID: 100, Start: 7}))
	if _, _, again, _ := f.host(f.worker); again != seen {
		t.Errorf("an unchanged host was written again: %d, then %d", seen, again)
	}
	for _, ignored := range []*wire.Host{nil, {PID: 101, Start: 0}, {PID: 0, Start: 8}, {PID: 1, Start: 8}, {PID: 101, Start: 8, NS: "other"}} {
		wantOK(t, f.command(f.worker, Who{}, "", ignored))
		if pid, _, _, _ := f.host(f.worker); pid != 100 {
			t.Errorf("host %+v replaced the stored one: pid %d", ignored, pid)
		}
	}
	wantOK(t, f.command(f.worker, Who{}, "", &wire.Host{PID: 101, Start: 8}))
	if pid, start, _, _ := f.host(f.worker); pid != 101 || start != 8 {
		t.Errorf("a resumed process did not replace the host: %d %d", pid, start)
	}
	wantOK(t, f.command(f.worker, Leave{}, "leave", &wire.Host{PID: 101, Start: 8}))
	if _, _, _, ok := f.host(f.worker); ok {
		t.Error("the host stayed after leave")
	}
}

func TestGoneHostDepartsTheMemberAndKeepsItsWork(t *testing.T) {
	f := newHostFixture(t)
	wantOK(t, f.command(f.lead, Ask{To: []string{"worker"}, Body: "review"}, "ask", &wire.Host{PID: 200, Start: 9}))
	f.sweep()
	if state, _, _ := f.bindingState(f.worker); state != "live" {
		t.Fatalf("a running host departed the member: %s", state)
	}
	f.gone[100] = true
	f.sweep()
	state, byHook, _ := f.bindingState(f.worker)
	if state != "departed" || !byHook {
		t.Fatalf("binding %s, by hook %v; want departed by a session end", state, byHook)
	}
	var detail string
	if err := f.db.QueryRow(`SELECT detail FROM event WHERE kind = 'session_ended' AND actor_id = ?`, f.principal(f.worker)).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	var d map[string]string
	if json.Unmarshal([]byte(detail), &d) != nil || d["by"] != "process" || d["alias"] != "worker" {
		t.Errorf("session_ended detail %s", detail)
	}
	if _, _, _, ok := f.host(f.worker); ok {
		t.Error("the host row stayed after the departure")
	}
	if n := f.count(`SELECT COUNT(*) FROM obligation WHERE principal_id = ? AND state = 'open'`, f.principal(f.worker)); n != 1 {
		t.Errorf("open requests owed by the departed member: %d, want 1", n)
	}
	// The alias is free for another session.
	other := f.newSession("codex")
	wantOK(t, f.hostJoin(other, "worker", nil))
	if state, _, _ := f.bindingState(f.lead); state != "live" {
		t.Error("the other member departed too")
	}
}

// Only an open launch the process sweep checks keeps a member from being
// judged by its host; any other is ended with the member.
func TestOnlyAProcessLaunchKeepsTheHostFromJudging(t *testing.T) {
	for _, c := range []struct {
		name, adapter, ref string
		verified, enabled  bool
		governs            bool
	}{
		{"Claude process launch", "claude", "pid-100-1", true, true, true},
		{"Claude process launch, adapter switched off", "claude", "pid-100-1", true, false, true},
		{"Codex session launch with its end hook enabled", "codex", SessionLaunch, false, true, false},
		{"Antigravity session launch", "agy", SessionLaunch, false, true, false},
		{"Claude launch without a launch ID", "claude", SessionLaunch, false, true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newHostFixture(t)
			f.policy(c.adapter, c.enabled, 1)
			worker := f.principal(f.worker)
			if _, err := f.db.Exec(`INSERT INTO adapter_launch (principal_id, launch_ref, profile, verified, started_hook_at_ms)
				VALUES (?, ?, json_object('adapter', ?), ?, 0)`, worker, c.ref, c.adapter, c.verified); err != nil {
				t.Fatal(err)
			}
			f.gone[100] = true
			f.sweep()
			state, _, _ := f.bindingState(f.worker)
			open := f.count(`SELECT COUNT(*) FROM adapter_launch WHERE principal_id = ? AND ended_hook_at_ms IS NULL`, worker)
			if c.governs && (state != "live" || open != 1) {
				t.Fatalf("binding %s, open launches %d; want live with its launch open", state, open)
			}
			if !c.governs && (state != "departed" || open != 0) {
				t.Fatalf("binding %s, open launches %d; want departed with its launch ended", state, open)
			}
		})
	}
}

// A hook-joined member whose launch nothing ends (Antigravity's) is tracked
// by its host. Its departure ends the launch: a start older than the
// departure is a stale callback, a later one brings the member back.
func TestHookJoinedMemberWithASessionLaunchDepartsByHost(t *testing.T) {
	f, writer := adapterChannel(t)
	h := &hostFixture{fixture: f, gone: map[int64]bool{}}
	f.svc.ProcessGone = func(pid, identity int64) bool { return h.gone[pid] }
	s := f.newSession("codex")
	t0 := f.now.UnixMilli()
	f.hook(s, start(SessionLaunch, "startup", t0))
	wantOK(t, h.command(s, Who{}, "", &wire.Host{PID: 400, Start: 3}))
	h.sweep()
	if state, _, _ := h.bindingState(s); state != "live" {
		t.Fatalf("a running host departed the member: %s", state)
	}
	h.gone[400] = true
	f.now = f.now.Add(5 * time.Second)
	h.sweep()
	if state, byHook, _ := h.bindingState(s); state != "departed" || !byHook {
		t.Fatalf("binding %s, by hook %v; want departed by its process", state, byHook)
	}
	if n := f.count(`SELECT COUNT(*) FROM adapter_launch WHERE ended_hook_at_ms = ?`, ms(f.now)); n != 1 {
		t.Fatalf("launches ended at the departure: %d", n)
	}
	f.hook(s, start(SessionLaunch, "resume", t0+1_000))
	if state, _, _ := h.bindingState(s); state != "departed" {
		t.Fatalf("a start older than the departure brought the member back: %s", state)
	}
	f.hook(s, start(SessionLaunch, "resume", ms(f.now)+1_000))
	if state, _, alias := h.bindingState(s); state != "live" || alias != "codex1" {
		t.Fatalf("binding %s as @%s; want live as @codex1 after a new start", state, alias)
	}
	if m := f.memberRow(writer, "codex1"); m.Alias != "codex1" {
		t.Fatalf("the member is not in the roster: %+v", m)
	}
}

// What the probe saw is checked again when its result is applied: a change
// in between departs nobody.
func TestAChangeAfterTheProbeDepartsNothing(t *testing.T) {
	for _, c := range []struct {
		name   string
		change func(f *hostFixture)
	}{
		{"the host changed", func(f *hostFixture) {
			wantOK(t, f.command(f.worker, Who{}, "", &wire.Host{PID: 101, Start: 8}))
		}},
		{"a launch opened", func(f *hostFixture) {
			f.exec1(`INSERT INTO adapter_launch (principal_id, launch_ref, profile, verified, started_hook_at_ms)
				VALUES (?, 'pid-100-1', '{"adapter":"claude"}', 1, 0)`, f.principal(f.worker))
		}},
		{"the binding's route changed", func(f *hostFixture) {
			f.exec1(`UPDATE binding SET route_epoch = route_epoch + 1 WHERE principal_id = ?`, f.principal(f.worker))
		}},
		{"the binding is no longer live", func(f *hostFixture) {
			f.exec1(`UPDATE binding SET state = 'departed', departed_by_owner = 1 WHERE principal_id = ?`, f.principal(f.worker))
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newHostFixture(t)
			tx, err := f.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			probed, err := HostedMembers(t.Context(), tx, 0, 32)
			if err != nil {
				t.Fatal(err)
			}
			tx.Rollback()
			worker := f.principal(f.worker)
			probed = slices.DeleteFunc(probed, func(m HostedMember) bool { return m.PrincipalID != worker })
			if len(probed) != 1 {
				t.Fatalf("probed %+v", probed)
			}
			c.change(f)
			tx, err = f.db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			changed, err := DepartGoneHosts(t.Context(), tx, probed, f.now)
			if err != nil || changed {
				t.Fatalf("changed %v, %v", changed, err)
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if n := f.count(`SELECT COUNT(*) FROM event WHERE kind = 'session_ended'`); n != 0 {
				t.Fatalf("a stale probe ended %d sessions", n)
			}
		})
	}
}

func (f *hostFixture) exec1(query string, args ...any) {
	f.t.Helper()
	if _, err := f.db.Exec(query, args...); err != nil {
		f.t.Fatal(err)
	}
}

func TestResumedSessionComesBackWithProof(t *testing.T) {
	f := newHostFixture(t)
	f.gone[100] = true
	f.sweep()
	resumed := &wire.Host{PID: 300, Start: 11}
	// Without a host, or with one that is gone, the command is refused.
	wantCode(t, f.command(f.worker, Inbox{}, "", nil), notEnrolled().Status, core.CodeNotEnrolled)
	f.gone[300] = true
	wantCode(t, f.command(f.worker, Inbox{}, "", resumed), notEnrolled().Status, core.CodeNotEnrolled)
	delete(f.gone, 300)
	r := f.command(f.worker, Inbox{}, "", resumed)
	wantOK(t, r)
	if state, _, alias := f.bindingState(f.worker); state != "live" || alias != "worker" {
		t.Fatalf("binding %s as @%s, want live as @worker", state, alias)
	}
	if fields := decode[map[string]any](t, r); fields["rejoined_as"] != nil {
		t.Errorf("an unchanged alias was announced: %v", fields["rejoined_as"])
	}
	if pid, _, _, _ := f.host(f.worker); pid != 300 {
		t.Errorf("host %d, want the resumed process", pid)
	}
	if n := f.count(`SELECT COUNT(*) FROM event WHERE kind = 'rejoined'`); n != 1 {
		t.Errorf("rejoined events: %d", n)
	}
}

func TestReturnTakesAnotherAliasWhenTheOldOneIsTaken(t *testing.T) {
	f := newHostFixture(t)
	f.gone[100] = true
	f.sweep()
	wantOK(t, f.hostJoin(f.newSession("codex"), "worker", nil))
	r := f.command(f.worker, Inbox{}, "", &wire.Host{PID: 300, Start: 11})
	wantOK(t, r)
	_, _, alias := f.bindingState(f.worker)
	if alias == "worker" {
		t.Fatal("came back under a taken alias")
	}
	if fields := decode[map[string]any](t, r); fields["rejoined_as"] != alias {
		t.Errorf("rejoined_as %v, want %s", fields["rejoined_as"], alias)
	}
}

func TestNoReturnIntoAnArchivedChannel(t *testing.T) {
	f := newHostFixture(t)
	f.gone[100] = true
	f.sweep()
	f.exec1(`UPDATE channel SET state = 'archived'`)
	wantCode(t, f.command(f.worker, Inbox{}, "", &wire.Host{PID: 300, Start: 11}), notEnrolled().Status, core.CodeNotEnrolled)
	if state, _, _ := f.bindingState(f.worker); state != "departed" {
		t.Fatalf("a member came back into an archived channel: %s", state)
	}
}

func TestNoReturnAfterLeavingOrAnOwnerDeparture(t *testing.T) {
	f := newHostFixture(t)
	alive := &wire.Host{PID: 300, Start: 11}
	wantOK(t, f.command(f.worker, Leave{}, "leave", &wire.Host{PID: 100, Start: 7}))
	wantCode(t, f.command(f.worker, Inbox{}, "", alive), notEnrolled().Status, core.CodeNotEnrolled)
	if state, _, _ := f.bindingState(f.worker); state != "departed" {
		t.Fatalf("a member that left came back: %s", state)
	}
	// What an owner departure leaves on the binding.
	if _, err := f.db.Exec(`UPDATE binding SET state = 'departed', departed_by_hook = 0, departed_by_owner = 1
		WHERE principal_id = ?`, f.principal(f.lead)); err != nil {
		t.Fatal(err)
	}
	wantCode(t, f.command(f.lead, Inbox{}, "", alive), notEnrolled().Status, core.CodeNotEnrolled)
	if state, _, _ := f.bindingState(f.lead); state != "departed" {
		t.Fatalf("a member the owner departed came back: %s", state)
	}
}

// A start that arrives after the old process died but before the sweep is
// positive evidence that the session runs elsewhere: it forgets the host,
// so the sweep departs nobody, and the next command names the new process.
func TestAResumedStartBeforeTheSweepKeepsTheMember(t *testing.T) {
	f, _ := adapterChannel(t)
	h := &hostFixture{fixture: f, gone: map[int64]bool{}}
	f.svc.ProcessGone = func(pid, identity int64) bool { return h.gone[pid] }
	s := f.newSession("codex")
	t0 := f.now.UnixMilli()
	f.hook(s, start(SessionLaunch, "startup", t0))
	wantOK(t, h.command(s, Who{}, "", &wire.Host{PID: 400, Start: 3}))
	h.gone[400] = true
	f.hook(s, start(SessionLaunch, "resume", t0+300))
	f.now = f.now.Add(500 * time.Millisecond)
	h.sweep()
	if state, _, _ := h.bindingState(s); state != "live" {
		t.Fatalf("a resumed session was departed with its old process: %s", state)
	}
	if n := f.count(`SELECT COUNT(*) FROM adapter_launch WHERE ended_hook_at_ms IS NULL`); n != 1 {
		t.Fatalf("open launches %d, want the resumed one", n)
	}
	wantOK(t, h.command(s, Who{}, "", &wire.Host{PID: 401, Start: 4}))
	if pid, _, _, ok := h.host(s); !ok || pid != 401 {
		t.Fatalf("host %d (%v), want the resumed process", pid, ok)
	}
}
