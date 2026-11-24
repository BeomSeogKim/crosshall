package domain

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// turn runs one owner-typed turn of the reader and returns its prompt's result.
func (p *publishFixture) turn() wire.HookResult {
	p.t.Helper()
	p.now = p.now.Add(time.Second)
	ref := fmt.Sprintf("turn-%d", p.now.UnixMilli())
	got := p.hook(p.reader, prompt(publishTestLaunch, ref, p.now.UnixMilli()))
	p.hook(p.reader, stop(publishTestLaunch, ref, p.now.UnixMilli()+1))
	return got
}

// wantReach checks the reach line of a result and stores it as the hook
// does once it has printed it.
func (p *publishFixture) wantReach(step string, got wire.HookResult, line string) {
	p.t.Helper()
	if got.ReachLine != line || (got.ReachAck == nil) != (line == "") {
		p.t.Fatalf("%s: reach line %q (ack %+v), want %q", step, got.ReachLine, got.ReachAck, line)
	}
	if line == "" {
		return
	}
	if want := map[string]int{ReachBringsLine: 1, ReachLostLine: 0}[line]; got.ReachAck.Told != want || got.ReachAck.BindingID != p.binding.ID {
		p.t.Fatalf("%s: ack %+v, want told %d for binding %d", step, got.ReachAck, want, p.binding.ID)
	}
	p.mutate(`UPDATE binding SET reach_told = ? WHERE id = ?`, got.ReachAck.Told, got.ReachAck.BindingID)
	if got.ReachAck.SuspensionID != 0 {
		p.mutate(`UPDATE publish_suspension SET recovery_hint = ? WHERE id = ?`,
			store.RecoveryHintAcknowledged, got.ReachAck.SuspensionID)
	}
}

// sweep runs the dispatcher tick's reach check.
func (p *publishFixture) sweep() {
	p.t.Helper()
	p.update(func(ctx context.Context, tx store.Tx) error {
		_, err := MarkReachChanges(ctx, tx, p.now)
		return err
	})
}

func (p *publishFixture) marked() bool {
	p.t.Helper()
	return p.count(`SELECT COUNT(*) FROM adapter_dirty WHERE principal_id = ? AND notice = 1`, p.principalOf(p.reader)) == 1
}

func (p *publishFixture) clearStamps() {
	p.t.Helper()
	p.mutate(`DELETE FROM adapter_dirty`)
}

// wantStampMove checks that what happened since clearStamps moved the
// reader's notice stamp, at its own event or at the next sweep, and that a
// further sweep does not move it again.
func (p *publishFixture) wantStampMove(step string, moved bool) {
	p.t.Helper()
	p.sweep()
	if got := p.marked(); got != moved {
		p.t.Fatalf("%s: stamp moved %t, want %t", step, got, moved)
	}
	p.clearStamps()
	p.sweep()
	if p.marked() {
		p.t.Fatalf("%s: the stamp moved again with nothing changed", step)
	}
}

func (p *publishFixture) told() any {
	p.t.Helper()
	var told any
	if err := p.db.QueryRow(`SELECT reach_told FROM binding WHERE id = ?`, p.binding.ID).Scan(&told); err != nil {
		p.t.Fatal(err)
	}
	return told
}

func TestReachLineIsSaidWhenItChangesAndOfferedUntilPrinted(t *testing.T) {
	p := newPublishFixture(t)
	p.wantReach("a route not yet proven", p.turn(), "")
	if told := p.told(); told != nil {
		t.Fatalf("nothing said, stored %v", told)
	}
	p.clearStamps()
	p.wantStampMove("never reached, never told", false)
	p.proveRoute()
	p.wantStampMove("proven", true)
	if got := p.turn(); got.ReachLine != ReachBringsLine {
		t.Fatalf("first reachable prompt: %q", got.ReachLine)
	}
	p.wantReach("the unprinted line is offered again", p.turn(), ReachBringsLine)
	p.wantReach("said once", p.turn(), "")

	// A queued notice still reaches the session, for HeldReachBound.
	p.turnEndAsk(p.writer, "reader", "held")
	id := p.reserve()
	ids := p.begin(id)
	if id == 0 || len(ids) == 0 {
		t.Fatal("no notice to hold")
	}
	p.resolve(id, PublishSubmitted)
	heldAt := p.now
	if s := p.publishState(); s.State != "held" {
		t.Fatalf("publish %+v, want held", s)
	}
	p.clearStamps()
	p.wantStampMove("held", false)
	p.wantReach("held", p.turn(), "")
	p.now = heldAt.Add(HeldReachBound - 2*time.Second)
	p.wantStampMove("held just under the bound", false)
	p.wantReach("held just under the bound", p.turn(), "")
	p.now = heldAt.Add(HeldReachBound)
	p.wantStampMove("held for the bound", true)
	p.wantReach("held for the bound", p.turn(), ReachLostLine)
	p.wantReach("said once", p.turn(), "")

	// The notice's own prompt sees the state after its receipt.
	p.now = p.now.Add(time.Second)
	p.clearStamps()
	got := p.hook(p.reader, p.promptReceipt("notice", publishTestLaunch, p.now.UnixMilli(), ids))
	p.observation(id, "own_turn")
	p.wantReach("the receipt ends the hold", got, ReachBringsLine)
	p.wantStampMove("told by the receipt's own prompt", false)
}

func TestReachLineFlipsWithEachWayTheHallStopsReaching(t *testing.T) {
	setAdapter := func(p *publishFixture, publish, key string) {
		wantOK(p.t, p.owner(AdapterSet{wire.AdapterPolicyArgs{Adapter: "claude", Publish: &publish}}, key))
	}
	// event marks a change that has a site of its own: it must move the
	// stamp in that transaction, before any sweep.
	for _, test := range []struct {
		name    string
		off, on func(p *publishFixture)
		event   bool
	}{
		{"absorbed receipt, lapsing", func(p *publishFixture) { p.observed("absorbed") },
			func(p *publishFixture) { p.now = p.now.Add(30 * time.Minute) }, true},
		{"build stop", func(p *publishFixture) {
			p.mutate(`INSERT INTO capability_build (vendor, vendor_version, platform, mode, manifest_rev, cap, stopped_at_ms, stop_reason, stop_ref)
				SELECT 'claude', json_extract(profile, '$.vendor_version'), json_extract(profile, '$.platform'),
					json_extract(profile, '$.mode'), json_extract(profile, '$.manifest_rev'), 'publish', ?, 'absorbed', 1
				FROM adapter_launch WHERE launch_ref = ?`, p.now.UnixMilli(), publishTestLaunch)
		}, func(p *publishFixture) {
			p.mutate(`UPDATE capability_build SET lifted_at_ms = ?`, p.now.UnixMilli()+1)
		}, false},
		{"hourly cap", func(p *publishFixture) {
			at := p.now.UnixMilli()
			p.mutate(`WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < ?)
				INSERT INTO publish_attempt (binding_id, principal_id, binding_epoch, generation, adapter, session_ref, launch_ref,
					policy_epoch, process_pid, process_start_ms, state, reserved_at_ms, submitting_at_ms, resolved_at_ms, observed_at_ms, observation)
				SELECT binding_id, principal_id, binding_epoch, generation, adapter, session_ref, launch_ref,
					policy_epoch, 42, ?, 'submitted', ?, ?, ?, ?, 'own_turn'
				FROM publish_route CROSS JOIN seq WHERE binding_id = ?`,
				store.PublishHourlyCap, p.queue.StartMS, at, at, at, at, p.binding.ID)
		}, func(p *publishFixture) { p.now = p.now.Add(time.Hour) }, false},
		{"owner hall switch", func(p *publishFixture) {
			wantOK(p.t, p.owner(OwnerPublish{Hall: true, Switch: "off"}, "hall-off"))
		}, func(p *publishFixture) {
			wantOK(p.t, p.owner(OwnerPublish{Hall: true, Switch: "on"}, "hall-on"))
		}, true},
		{"owner adapter switch", func(p *publishFixture) { setAdapter(p, "off", "adapter-off") },
			func(p *publishFixture) { setAdapter(p, "default", "adapter-default") }, true},
		{"owner member switch", func(p *publishFixture) {
			wantOK(p.t, p.owner(OwnerPublish{BindingID: p.binding.ID, Switch: "off"}, "member-off"))
		}, func(p *publishFixture) {
			wantOK(p.t, p.owner(OwnerPublish{BindingID: p.binding.ID, Switch: "default"}, "member-default"))
		}, true},
		{"member's own switch", func(p *publishFixture) {
			wantOK(p.t, p.run(p.reader, Publish{}, "self-off"))
		}, func(p *publishFixture) {
			wantOK(p.t, p.run(p.reader, Publish{On: true}, "self-on"))
		}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := newPublishFixture(t)
			p.proveRoute()
			p.wantStampMove("proven", true)
			p.wantReach("reachable", p.turn(), ReachBringsLine)
			p.clearStamps()
			test.off(p)
			if s := p.publishState(); ReachesForNewWork(s, p.now) {
				t.Fatalf("publish %+v still reaches", s)
			}
			if test.event && !p.marked() {
				t.Fatal("the change did not move the stamp in its own transaction")
			}
			p.wantStampMove("stopped reaching", true)
			p.wantReach("stopped reaching", p.turn(), ReachLostLine)
			p.wantReach("said once", p.turn(), "")
			p.clearStamps()
			test.on(p)
			if s := p.publishState(); s.State != "available" {
				t.Fatalf("publish %+v, want available again", s)
			}
			p.wantStampMove("back", true)
			p.wantReach("back", p.turn(), ReachBringsLine)
		})
	}
}

func TestReachLossNamesOwnerResumeForLastingLaunchSuspension(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	p.wantReach("available route", p.turn(), ReachBringsLine)
	id := p.observed("absorbed")
	p.mutate(`UPDATE publish_suspension SET until_ms = NULL WHERE notice = ?`, id)
	want := fmt.Sprintf("%s Your user can turn notices back on with crosshall owner publish resume %d.", ReachLostLine, p.binding.ID)
	p.wantReach("lasting launch suspension", p.turn(), want)
	p.wantReach("the hint was already printed", p.turn(), "")
}

func TestReachOffersRecoveryAfterGenericLossWasAcknowledged(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	p.wantReach("available route", p.turn(), ReachBringsLine)
	id := p.observed("absorbed")
	p.wantReach("loss acknowledged by the previous dispatcher", p.turn(), ReachLostLine)
	p.clearStamps()
	p.mutate(`UPDATE publish_suspension SET until_ms = NULL WHERE notice = ?`, id)
	p.wantStampMove("the old lasting suspension has an unsent hint", true)
	command := fmt.Sprintf("crosshall owner publish resume %d", p.binding.ID)
	result := p.ask(p.writer, Ask{To: []string{"reader"}, Body: "while suspended"}, "sender-status")
	wantOK(t, result)
	if strings.Contains(string(result.Data), command) {
		t.Fatalf("sender saw the affected session's recovery command: %s", result.Data)
	}
	want := ReachLostLine + " Your user can turn notices back on with " + command + "."
	if got := p.turn(); got.ReachLine != want {
		t.Fatalf("first offer: %q, want %q", got.ReachLine, want)
	}
	p.wantReach("offer again until printed", p.turn(), want)
	p.wantReach("stop after acknowledgment", p.turn(), "")
}

func TestReachRearmsUnprintedRecoveryWhenPublishingReturns(t *testing.T) {
	for _, test := range []struct {
		name    string
		printed bool
	}{{"unprinted", false}, {"already printed", true}} {
		t.Run(test.name, func(t *testing.T) {
			p := newPublishFixture(t)
			p.proveRoute()
			p.wantReach("available route", p.turn(), ReachBringsLine)
			id := p.observed("absorbed")
			p.wantReach("generic loss acknowledged", p.turn(), ReachLostLine)
			p.clearStamps()
			p.mutate(`UPDATE publish_suspension SET until_ms = NULL WHERE notice = ?`, id)
			p.sweep()
			if !p.marked() {
				t.Fatal("the pending hint did not raise the notice stamp")
			}
			hint := fmt.Sprintf("%s Your user can turn notices back on with crosshall owner publish resume %d.", ReachLostLine, p.binding.ID)
			if test.printed {
				p.wantReach("hint printed before switch off", p.turn(), hint)
			}
			wantOK(t, p.owner(OwnerPublish{BindingID: p.binding.ID, Switch: "off"}, "recovery-off"))
			got := p.turn()
			p.wantReach("the off switch hides the hint", got, "")
			if got.Ack == nil || got.Ack.Kind != "notice" {
				t.Fatalf("a prompt while off could not consume the notice stamp: %+v", got)
			}
			p.clearStamps()
			wantOK(t, p.owner(OwnerPublish{BindingID: p.binding.ID, Switch: "default"}, "recovery-default"))
			p.wantStampMove("publishing restored", !test.printed)
			if test.printed {
				hint = ""
			}
			p.wantReach("next prompt after restoring publishing", p.turn(), hint)
			p.wantReach("after print acknowledgment", p.turn(), "")
		})
	}
}

func TestReachRecoveryWithAStoppedBuildNamesAWorkingCommand(t *testing.T) {
	p := newProbationFixture(t)
	p.wantReach("available route", p.turn(), ReachBringsLine)
	at := p.now.UnixMilli()
	id, ids := probationNotice(p, p.binding, "reader", "stopped-build")
	p.hook(p.reader, prompt(publishTestLaunch, "already-open", at+1))
	p.hook(p.reader, p.promptReceipt("already-open", publishTestLaunch, at+2, ids))
	p.observation(id, "absorbed")
	probationBuild(p, 0, id, "absorbed", false)
	p.mutate(`UPDATE publish_suspension SET until_ms = NULL WHERE notice = ?`, id)
	want := fmt.Sprintf("%s Your user can turn notices back on with crosshall owner publish resume %d.", ReachLostLine, p.binding.ID)
	p.wantReach("both the build and the launch are stopped", p.turn(), want)
	p.now = p.now.Add(time.Second)
	enrollAndRoute(t, p.fixture, p.reader, "pid-43-restart", "resume", publishTestSocket, "default", p.now.UnixMilli(), 1)
	p.proveRoute()
	p.requirePublish("a fresh launch still has the build stop", "suspended", "version")
	wantOK(t, p.owner(OwnerPublish{BindingID: p.binding.ID, Switch: "resume"}, "resume-both"))
	p.turnEndAsk(p.writer, "reader", "after-resume")
	p.requirePublish("owner resume lifts the build stop", "available", "")
}

func TestReachLineRespectsDoNotDisturb(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	p.wantReach("reachable", p.turn(), ReachBringsLine)
	p.mutate(`UPDATE binding SET dnd = 1 WHERE id = ?`, p.binding.ID)
	p.wantReach("do not disturb", p.turn(), "")
	if told := p.told(); told != int64(1) {
		t.Fatalf("do not disturb changed the stored value to %v", told)
	}
	p.mutate(`UPDATE binding SET dnd = 0 WHERE id = ?`, p.binding.ID)
	p.wantReach("after do not disturb", p.turn(), "")
}

// A start registers a new route generation, which the hall cannot use until
// it is proven again: the start says so, and the next prompt after the
// proof says "brings" again.
func TestReachLineAcrossAContextReset(t *testing.T) {
	for _, source := range []string{"clear", "compact", "resume"} {
		t.Run(source, func(t *testing.T) {
			p := newPublishFixture(t)
			p.proveRoute()
			p.wantReach("reachable", p.turn(), ReachBringsLine)
			p.now = p.now.Add(time.Second)
			at := p.now.UnixMilli()
			got := p.hook(p.reader, start(publishTestLaunch, source, at))
			if got.Enrollment != "restored" {
				t.Fatalf("%s start: %+v", source, got)
			}
			p.wantReach("start before the new route is proven", got, ReachLostLine)
			wantOK(t, p.exec(p.caller(p.reader), Request{
				Command: claudeRoute(publishTestLaunch, fmt.Sprintf("route:%s:%d", source, at), publishTestSocket, "default", at, 1),
				Vendor:  p.reader.vendor, SessionKey: p.reader.key,
				OpKey: fmt.Sprintf("hook:claude:%s:route:%s:%d", publishTestLaunch, source, at),
			}))
			p.proveRoute()
			p.wantReach("proven again", p.turn(), ReachBringsLine)
		})
	}
}

// After a reset, a member that was told "brings" and is still reached is
// told again: its new context has not seen the line.
func TestReachLineIsRepeatedToARestoredContext(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	p.wantReach("reachable", p.turn(), ReachBringsLine)
	principal := p.principalOf(p.reader)
	for _, restored := range []bool{false, true} {
		var out wire.HookResult
		p.update(func(ctx context.Context, tx store.Tx) error {
			b, _, err := bindingOf(ctx, tx, principal)
			if err != nil {
				return err
			}
			return offerReach(ctx, tx, b, restored, &out, p.now)
		})
		want := map[bool]string{false: "", true: ReachBringsLine}[restored]
		p.wantReach(fmt.Sprintf("restored %t", restored), out, want)
	}
}

func TestReachLineNeverReachesAMemberWithoutARoute(t *testing.T) {
	f, writer := adapterChannel(t)
	codex := f.newSession("codex")
	wantOK(t, f.join(codex, Join{Channel: "checkout", Alias: "coder"}, "coder-join"))

	f.turnEndAsk(writer, "coder", "work")
	at := f.now.UnixMilli()
	f.hook(codex, start("pid-77-start", "startup", at))
	for i := range 3 {
		ref := fmt.Sprintf("codex-%d", i)
		got := f.hook(codex, prompt("pid-77-start", ref, at+int64(10*(i+1))))
		if got.ReachLine != "" || got.ReachAck != nil {
			t.Fatalf("prompt %d of a member the hall never reaches: %+v", i, got)
		}
		f.hook(codex, stop("pid-77-start", ref, at+int64(10*(i+1))+1))
	}
}

// A member the hall never reached and never told keeps the hook's clean path
// through every change: none of them moves its stamp.
func TestReachNeverMovesTheStampOfAMemberNeverReachedOrTold(t *testing.T) {
	p := newPublishFixture(t) // a route that is never proven
	p.clearStamps()
	off := "off"
	for i, change := range []func(){
		func() { wantOK(t, p.owner(OwnerPublish{Hall: true, Switch: "off"}, "hall-off")) },
		func() { wantOK(t, p.owner(OwnerPublish{Hall: true, Switch: "on"}, "hall-on")) },
		func() {
			wantOK(t, p.owner(AdapterSet{wire.AdapterPolicyArgs{Adapter: "claude", Publish: &off}}, "adapter-off"))
		},
		func() { wantOK(t, p.owner(OwnerPublish{BindingID: p.binding.ID, Switch: "off"}, "member-off")) },
		func() { wantOK(t, p.owner(OwnerPublish{BindingID: p.binding.ID, Switch: "default"}, "member-default")) },
		func() { wantOK(t, p.run(p.reader, Publish{}, "self-off")) },
		func() { wantOK(t, p.run(p.reader, Publish{On: true}, "self-on")) },
		func() { p.now = p.now.Add(time.Hour) },
	} {
		change()
		p.wantStampMove(fmt.Sprintf("change %d", i), false)
	}
	if told := p.told(); told != nil {
		t.Fatalf("a member never reached was told %v", told)
	}
}
