package domain

import (
	"context"
	"fmt"
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
	p.proveRoute()
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
	p.wantReach("held", p.turn(), "")
	p.now = heldAt.Add(HeldReachBound - 2*time.Second)
	p.wantReach("held just under the bound", p.turn(), "")
	p.wantReach("held for the bound", p.turn(), ReachLostLine)
	p.wantReach("said once", p.turn(), "")

	// The notice's own prompt sees the state after its receipt.
	p.now = p.now.Add(time.Second)
	got := p.hook(p.reader, p.promptReceipt("notice", publishTestLaunch, p.now.UnixMilli(), ids))
	p.observation(id, "own_turn")
	p.wantReach("the receipt ends the hold", got, ReachBringsLine)
}

func TestReachLineFlipsWithEachWayTheHallStopsReaching(t *testing.T) {
	for _, test := range []struct {
		name    string
		off, on func(p *publishFixture)
	}{
		{"absorbed receipt, lapsing", func(p *publishFixture) { p.observed("absorbed") },
			func(p *publishFixture) { p.now = p.now.Add(30 * time.Minute) }},
		{"build stop", func(p *publishFixture) {
			p.mutate(`INSERT INTO capability_build (vendor, vendor_version, platform, mode, manifest_rev, cap, stopped_at_ms, stop_reason, stop_ref)
				SELECT 'claude', json_extract(profile, '$.vendor_version'), json_extract(profile, '$.platform'),
					json_extract(profile, '$.mode'), json_extract(profile, '$.manifest_rev'), 'publish', ?, 'absorbed', 1
				FROM adapter_launch WHERE launch_ref = ?`, p.now.UnixMilli(), publishTestLaunch)
		}, func(p *publishFixture) {
			p.mutate(`UPDATE capability_build SET lifted_at_ms = ?`, p.now.UnixMilli()+1)
		}},
		{"hourly cap", func(p *publishFixture) {
			at := p.now.UnixMilli()
			p.mutate(`WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < ?)
				INSERT INTO publish_attempt (binding_id, principal_id, binding_epoch, generation, adapter, session_ref, launch_ref,
					policy_epoch, process_pid, process_start_ms, state, reserved_at_ms, submitting_at_ms, resolved_at_ms, observed_at_ms, observation)
				SELECT binding_id, principal_id, binding_epoch, generation, adapter, session_ref, launch_ref,
					policy_epoch, 42, ?, 'submitted', ?, ?, ?, ?, 'own_turn'
				FROM publish_route CROSS JOIN seq WHERE binding_id = ?`,
				store.PublishHourlyCap, p.queue.StartMS, at, at, at, at, p.binding.ID)
		}, func(p *publishFixture) { p.now = p.now.Add(time.Hour) }},
		{"owner hall switch", func(p *publishFixture) {
			wantOK(p.t, p.owner(OwnerPublish{Hall: true, Switch: "off"}, "hall-off"))
		}, func(p *publishFixture) {
			wantOK(p.t, p.owner(OwnerPublish{Hall: true, Switch: "on"}, "hall-on"))
		}},
		{"owner adapter switch", func(p *publishFixture) {
			p.mutate(`UPDATE adapter_policy SET publish = 0 WHERE adapter = 'claude'`)
		}, func(p *publishFixture) {
			p.mutate(`UPDATE adapter_policy SET publish = NULL WHERE adapter = 'claude'`)
		}},
		{"owner member switch", func(p *publishFixture) {
			wantOK(p.t, p.owner(OwnerPublish{BindingID: p.binding.ID, Switch: "off"}, "member-off"))
		}, func(p *publishFixture) {
			wantOK(p.t, p.owner(OwnerPublish{BindingID: p.binding.ID, Switch: "default"}, "member-default"))
		}},
		{"member's own switch", func(p *publishFixture) {
			wantOK(p.t, p.run(p.reader, Publish{}, "self-off"))
		}, func(p *publishFixture) {
			wantOK(p.t, p.run(p.reader, Publish{On: true}, "self-on"))
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := newPublishFixture(t)
			p.proveRoute()
			p.wantReach("reachable", p.turn(), ReachBringsLine)
			test.off(p)
			if s := p.publishState(); ReachesForNewWork(s, p.now) {
				t.Fatalf("publish %+v still reaches", s)
			}
			p.wantReach("stopped reaching", p.turn(), ReachLostLine)
			p.wantReach("said once", p.turn(), "")
			test.on(p)
			if s := p.publishState(); s.State != "available" {
				t.Fatalf("publish %+v, want available again", s)
			}
			p.wantReach("back", p.turn(), ReachBringsLine)
		})
	}
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
	f.roots(writer, 4)
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
