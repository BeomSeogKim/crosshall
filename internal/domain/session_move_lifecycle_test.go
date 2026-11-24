package domain

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func TestHooksFollowTheMovedMembership(t *testing.T) {
	f, _, member, _, unread := clearFixture(t, true)
	principal := f.principalOf(member)
	wantOK(t, f.owner(CreateChannel{Name: "review"}, "create-review"))
	peer := f.newSession("claude")
	wantOK(t, f.join(peer, Join{Channel: "review", Alias: "peer"}, "join-peer"))
	f.now = f.now.Add(time.Second)
	wantOK(t, f.join(member, Join{Channel: "review", Alias: "moved", Move: true}, "move-review"))
	binding := f.count(`SELECT binding_id FROM member_selection WHERE principal_id = ?`, principal)
	if ended := f.count(`SELECT COUNT(*) FROM adapter_launch WHERE principal_id = ? AND launch_ref = 'pid-7-a' AND ended_hook_at_ms IS NOT NULL`, principal); ended != 0 {
		t.Fatal("moving ended the running launch")
	}
	request := decode[AskResult](t, f.ask(peer, Ask{To: []string{"moved"}, Body: "review this"}, "ask-review")).ID
	got := f.hook(member, prompt("pid-7-a", "moved-turn", f.now.UnixMilli()+1))
	if got.OfferID == 0 || f.count(`SELECT COUNT(*) FROM offer o JOIN offer_item i ON i.offer_id = o.id
		WHERE o.id = ? AND o.binding_id = ? AND i.message_id = ?`, got.OfferID, binding, request) != 1 {
		t.Fatalf("prompt on the moved launch did not offer the destination request: %+v", got)
	}
	if f.count(`SELECT COUNT(*) FROM offer_item WHERE offer_id = ? AND message_id = ?`, got.OfferID, unread) != 0 {
		t.Fatal("the moved prompt offered source work")
	}
	f.hook(member, ended("pid-7-a", f.now.UnixMilli()+1))
	if f.count(`SELECT COUNT(*) FROM binding WHERE id = ? AND state = 'departed' AND departed_by_hook = 1`, binding) != 1 {
		t.Fatal("the session end did not depart the destination binding")
	}
	resumedAt := f.now.UnixMilli() + 1
	f.now = f.now.Add(time.Second)
	got = f.hook(member, start("pid-8-b", "resume", resumedAt))
	if got.Enrollment != "restored" || got.Channel != "review" || got.Alias != "moved" {
		t.Fatalf("resume from the source repository restored the wrong selection: %+v", got)
	}
	if got := f.hook(member, prompt("pid-8-b", "resumed-turn", resumedAt+1)); got.Ack == nil {
		t.Fatalf("a prompt captured after the resumed start was dropped: %+v", got)
	}
	if f.count(`SELECT COUNT(*) FROM binding WHERE principal_id = ? AND id <> ? AND state = 'live'`, principal, binding) != 0 {
		t.Fatal("a hook revived the historical source binding")
	}
}

func TestCallbacksAtOrBeforeCutoverCannotUseTheNewMembership(t *testing.T) {
	f, _, member, _, _ := clearFixture(t, true)
	principal := f.principalOf(member)
	wantOK(t, f.owner(CreateChannel{Name: "review"}, "create-review"))
	f.now = f.now.Add(time.Second)
	wantOK(t, f.join(member, Join{Channel: "review", Alias: "moved", Move: true}, "move-review"))
	f.now = f.now.Add(time.Second)
	cutover := f.now.UnixMilli()
	wantOK(t, f.join(member, Join{Channel: "checkout", Alias: "claude1", Move: true}, "move-back"))
	var membership core.Membership
	if err := f.db.QueryRow(`SELECT binding_id, generation FROM member_selection WHERE principal_id = ?`, principal).
		Scan(&membership.BindingID, &membership.Generation); err != nil {
		t.Fatal(err)
	}
	caller := core.Caller{Role: core.RoleMember, PrincipalID: &principal, BindingID: &membership.BindingID, Membership: &membership}
	runHook := func(key string, command Command) wire.HookResult {
		r := f.exec(caller, Request{OpKey: key, Command: command, Vendor: member.vendor, SessionKey: member.key})
		wantOK(t, r)
		return decode[wire.HookResult](t, r)
	}
	for _, captured := range []int64{cutover - 1, cutover} {
		for i, cmd := range []Command{
			prompt("pid-7-a", "old-prompt", captured),
			stop("pid-7-a", "old-stop", captured),
			ended("pid-7-a", captured),
			start("pid-7-a", "resume", captured),
		} {
			key := fmt.Sprintf("late:%d:%d", captured, i)
			if got := runHook(key, cmd); !quiet(got) {
				t.Fatalf("stale %s captured at %d printed or acknowledged: %+v", cmd.Verb(), captured, got)
			}
			if n := f.count(`SELECT COUNT(*) FROM op WHERE principal_id = ? AND op_key = ?`, principal, key); n != 0 {
				t.Fatalf("stale %s captured at %d claimed an operation", cmd.Verb(), captured)
			}
		}
	}
	if n := f.count(`SELECT COUNT(*) FROM binding WHERE id = ? AND state = 'live'`, membership.BindingID); n != 1 {
		t.Fatal("a late end departed the returned member")
	}
	got := runHook("fresh-start", start("pid-7-a", "resume", cutover+1))
	if got.Enrollment != "restored" || got.Channel != "checkout" {
		t.Fatalf("fresh start did not restore the selected channel: %+v", got)
	}
	got = runHook("fresh-prompt", prompt("pid-7-a", "fresh-turn", cutover+2))
	if got.Ack == nil {
		t.Fatalf("fresh prompt could not offer the selected channel's work: %+v", got)
	}
	runHook("fresh-stop", stop("pid-7-a", "fresh-turn", cutover+3))
	if n := f.count(`SELECT COUNT(*) FROM observation WHERE principal_id = ? AND event_ref = 'fresh-turn:stop'`, principal); n != 1 {
		t.Fatal("fresh stop was not applied")
	}
	runHook("fresh-end", ended("pid-7-a", cutover+4))
	if n := f.count(`SELECT COUNT(*) FROM binding WHERE id = ? AND state = 'departed'`, membership.BindingID); n != 1 {
		t.Fatal("fresh end did not depart the selected channel")
	}
}

func TestRouteCallbackAtCutoverDoesNotAttachToTheNewBinding(t *testing.T) {
	f, _, member, _, _ := clearFixture(t, true)
	principal := f.principalOf(member)
	wantOK(t, f.owner(CreateChannel{Name: "review"}, "create-review"))
	f.now = f.now.Add(time.Second)
	cutover := f.now.UnixMilli()
	wantOK(t, f.join(member, Join{Channel: "review", Alias: "moved", Move: true}, "move-review"))
	var membership core.Membership
	if err := f.db.QueryRow(`SELECT binding_id, generation FROM member_selection WHERE principal_id = ?`, principal).
		Scan(&membership.BindingID, &membership.Generation); err != nil {
		t.Fatal(err)
	}
	caller := core.Caller{Role: core.RoleMember, PrincipalID: &principal, BindingID: &membership.BindingID, Membership: &membership}
	for _, key := range []string{"old-route", "fresh-route"} {
		cmd := claudeRoute("pid-7-a", key, "/tmp/cc-socks/7.sock", "default", cutover, 1)
		if key == "fresh-route" {
			cmd.HookAtMS++
		}
		cmd.Start = true
		wantOK(t, f.exec(caller, Request{OpKey: key, Command: cmd, Vendor: member.vendor, SessionKey: member.key}))
		if n := f.count(`SELECT COUNT(*) FROM op WHERE principal_id = ? AND op_key = ?`, principal, key); (n > 0) != (key == "fresh-route") {
			t.Fatalf("unexpected route claim at %d for %s: %d", cmd.HookAtMS, key, n)
		}
	}
}

func TestClearBeforeMoveCannotTakeOverTheNewSelection(t *testing.T) {
	f, _, member, _, _ := clearFixture(t, true)
	wantOK(t, f.owner(CreateChannel{Name: "review"}, "create-review"))
	f.now = f.now.Add(time.Second)
	at := f.now.UnixMilli()
	wantOK(t, f.join(member, Join{Channel: "review", Alias: "moved", Move: true}, "move-review"))
	for _, captured := range []int64{at - 1, at} {
		old := f.newSession("claude")
		got := f.hook(old, start("pid-7-b", "clear", captured))
		if !quiet(got) || f.keyState(member) != "live" {
			t.Fatalf("clear captured at %d around cutover %d displaced the member: %+v", captured, at, got)
		}
	}
	cleared := f.newSession("claude")
	got := f.hook(cleared, start("pid-7-c", "clear", at+1))
	if got.Enrollment != "restored" || got.Channel != "review" || got.Alias != "moved" || f.keyState(member) != "retired" {
		t.Fatalf("clear after the move did not restore the destination: %+v", got)
	}
}

func TestClearAtSameMillisecondAsStartAndMoveDoesNotJoinTheSource(t *testing.T) {
	f, _, member, _, _ := clearFixture(t, true)
	wantOK(t, f.owner(CreateChannel{Name: "review"}, "create-review"))
	at := f.now.UnixMilli()
	wantOK(t, f.join(member, Join{Channel: "review", Alias: "moved", Move: true}, "move-review"))
	old := f.newSession("claude")
	if got := f.hook(old, start("pid-7-b", "clear", at)); !quiet(got) || f.caller(old).PrincipalID != nil {
		t.Fatalf("an ambiguous clear joined a channel after the move: %+v", got)
	}
}

func TestOwnerRestoreRequiresTheSelectedBindingAfterAMove(t *testing.T) {
	f, member, _, _ := channel3(t)
	f.svc.RestoreKey = ident.RestoreKey
	principal := f.principalOf(member)
	source := int64(f.count(`SELECT id FROM binding WHERE principal_id = ?`, principal))
	wantOK(t, f.owner(CreateChannel{Name: "review"}, "create-review"))
	wantOK(t, f.join(member, Join{Channel: "review", Alias: "moved", Move: true}, "move-review"))
	destination := int64(f.count(`SELECT binding_id FROM member_selection WHERE principal_id = ?`, principal))
	wantOK(t, f.join(member, Join{Channel: "checkout", Move: true}, "move-back"))
	wantOK(t, f.run(member, Leave{}, "leave-checkout"))
	replacement := f.newSession("codex")
	cmd := Restore{BindingID: destination, Vendor: replacement.vendor, Key: replacement.key}
	refusal := f.owner(cmd, "restore-history")
	wantCode(t, refusal, core.ExitRefused, core.CodeBindingChanged)
	if hint := decode[Refusal](t, refusal).Hint; !strings.Contains(hint, fmt.Sprintf("crosshall owner restore %d", source)) {
		t.Fatalf("historical restore hint %q does not name selected binding %d", hint, source)
	}
	if f.keyState(member) != "live" {
		t.Fatal("the historical restore retired the live identity")
	}
	cmd.BindingID = source
	wantOK(t, f.owner(cmd, "restore-selected-departed"))
	if f.keyState(replacement) != "live" || f.keyState(member) != "retired" {
		t.Fatal("restore no longer works for the selected departed binding")
	}
}

func TestHostProbeCannotDepartAMemberAfterMovingAwayAndBack(t *testing.T) {
	f := newHostFixture(t)
	principal := f.principal(f.worker)
	old, err := HostedMembers(t.Context(), f.db, principal-1, 1)
	if err != nil || len(old) != 1 || old[0].PrincipalID != principal {
		t.Fatalf("no initial host probe: %+v (%v)", old, err)
	}
	wantOK(t, f.owner(CreateChannel{Name: "review"}, "create-review"))
	host := &wire.Host{PID: old[0].PID, Start: old[0].StartIdentity}
	wantOK(t, f.command(f.worker, Join{Channel: "review", Alias: "moved", Move: true}, "move-review", host))
	moved, err := HostedMembers(t.Context(), f.db, principal-1, 1)
	if err != nil || len(moved) != 1 || moved[0].PrincipalID != principal || moved[0].BindingID == old[0].BindingID || moved[0].PID != old[0].PID {
		t.Fatalf("host probe did not follow the destination: %+v (%v)", moved, err)
	}
	wantOK(t, f.command(f.worker, Join{Channel: "dev", Alias: "worker", Move: true}, "move-back", host))
	if changed, err := DepartGoneHosts(t.Context(), f.db, old, f.now); err != nil || changed {
		t.Fatalf("a probe from before the move departed the returned member: changed=%v, error=%v", changed, err)
	}
	f.gone[host.PID] = true
	f.sweep()
	if f.count(`SELECT COUNT(*) FROM binding WHERE id = ? AND state = 'departed' AND departed_by_hook = 1`, old[0].BindingID) != 1 {
		t.Fatal("a fresh process observation failed to depart the returned member")
	}
}

func TestClaudeProbeFollowsTheSelectionAndFencesTheEarlierGeneration(t *testing.T) {
	f, _, member, _, _ := clearFixture(t, true)
	old := liveClaudeLaunches(f, 0, 1)[0]
	wantOK(t, f.owner(CreateChannel{Name: "review"}, "create-review"))
	wantOK(t, f.join(member, Join{Channel: "review", Alias: "moved", Move: true}, "move-review"))
	moved := liveClaudeLaunches(f, 0, 1)
	if len(moved) != 1 || moved[0].BindingID == old.BindingID || moved[0].LaunchRef != old.LaunchRef || moved[0].MembershipGeneration <= old.MembershipGeneration {
		t.Fatalf("launch probe did not follow the destination: %+v", moved)
	}
	wantOK(t, f.join(member, Join{Channel: "checkout", Alias: "claude1", Move: true}, "move-back"))
	if applyClaudeProbes(f, ClaudeLaunchProbe{ClaudeLaunch: old, Gone: true}) {
		t.Fatal("an old launch probe departed the returned member")
	}
	current := liveClaudeLaunches(f, 0, 1)[0]
	if current.BindingID != old.BindingID || !applyClaudeProbes(f, ClaudeLaunchProbe{ClaudeLaunch: current, Gone: true}) {
		t.Fatal("a fresh launch probe did not depart the returned member")
	}
}
