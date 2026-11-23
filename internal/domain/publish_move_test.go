package domain

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/notice"
	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func (p *publishFixture) selectMoveBinding() {
	p.t.Helper()
	if err := p.db.QueryRow(`SELECT binding_id FROM member_selection WHERE principal_id = ?`, p.principalOf(p.reader)).
		Scan(&p.binding.ID); err != nil {
		p.t.Fatal(err)
	}
	p.refreshBinding()
}

func (p *publishFixture) attachMovedSocket(key string) {
	p.t.Helper()
	p.now = p.now.Add(time.Millisecond)
	wantOK(p.t, p.exec(p.caller(p.reader), Request{
		Command: claudeRoute(publishTestLaunch, key, publishTestSocket, "default", p.now.UnixMilli(), 1),
		Vendor:  p.reader.vendor, SessionKey: p.reader.key, OpKey: key,
	}))
	p.refreshBinding()
}

func TestPublishMoveCancelsReservationAndRequiresFreshProofOnReturn(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	source := p.binding
	wantOK(t, p.owner(CreateChannel{Name: "delivery"}, "create-delivery"))
	p.turnEndAsk(p.writer, "reader", "reserved-at-source")
	id := p.reserve()
	if id == 0 {
		t.Fatal("no source reservation")
	}
	wantOK(t, p.join(p.reader, Join{Channel: "delivery", Alias: "reader", Move: true}, "move-out"))
	p.selectMoveBinding()
	p.state(id, "not_submitted")
	if n := p.count(`SELECT COUNT(*) FROM announcement WHERE via = 'publish' AND ref = ?`, id); n != 0 {
		t.Fatal("the unfenced reservation kept its announcement claims")
	}
	if n := p.count(`SELECT COUNT(*) FROM publish_route_proof WHERE binding_id = ? AND generation = ? AND activated_at_ms IS NOT NULL`, source.ID, source.Generation); n != 1 {
		t.Fatal("the old route proof was reattributed or removed")
	}
	if n := p.count(`SELECT COUNT(*) FROM publish_route WHERE binding_id = ? AND socket_path IS NULL AND state <> 'active'`, p.binding.ID); n != 1 {
		t.Fatal("the destination reused the source socket")
	}
	p.attachMovedSocket("destination-socket")
	p.proveRoute()
	wantOK(t, p.join(p.reader, Join{Channel: "checkout", Move: true}, "move-back"))
	p.selectMoveBinding()
	if p.binding.ID != source.ID {
		t.Fatalf("return selected binding %d, want %d", p.binding.ID, source.ID)
	}
	p.attachMovedSocket("return-socket")
	p.update(func(ctx context.Context, tx store.Tx) error {
		return ConfirmPublishRoute(ctx, tx, p.binding, p.queue)
	})
	p.turnEndAsk(p.writer, "reader", "after-new-proof")
	p.requirePublish("proof still needs activation", "candidate", store.PublishNotActivated)
	p.activate()
	p.turnEndAsk(p.writer, "reader", "after-new-activation")
	p.requirePublish("the new binding epoch was proved and activated", "available", "")
}

func TestPublishMoveWaitsForReceiptOnVoluntarilyDepartedBinding(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	p.finishPreviousTurn()
	source := p.binding.ID
	wantOK(t, p.owner(CreateChannel{Name: "delivery"}, "create-delivery"))
	p.turnEndAsk(p.writer, "reader", "submitted-at-source")
	id := p.reserve()
	ids := p.begin(id)
	if id == 0 || len(ids) == 0 {
		t.Fatal("no fenced source attempt")
	}
	move := Join{Channel: "delivery", Alias: "reader", Move: true}
	wantCode(t, p.join(p.reader, move, "move-before-submit"), core.ExitRefused, core.CodeMoveNoticePending)
	p.resolve(id, PublishUncertain)
	wantOK(t, p.run(p.reader, Leave{}, "leave-source"))
	move.Move = false
	wantCode(t, p.join(p.reader, move, "move-before-receipt"), core.ExitRefused, core.CodeMoveNoticePending)
	if n := p.count(`SELECT COUNT(*) FROM member_selection WHERE binding_id = ?`, source); n != 1 {
		t.Fatal("refused move changed the selected binding")
	}
	p.now = p.now.Add(time.Second)
	receipt := p.promptReceipt("late-notice", publishTestLaunch, p.now.UnixMilli(), ids)
	principal := p.principalOf(p.reader)
	response := p.exec(core.Caller{Role: core.RoleUnenrolled, PrincipalID: &principal}, Request{
		Command: receipt, Vendor: p.reader.vendor, SessionKey: p.reader.key, OpKey: hookKey(receipt),
	})
	if got := decode[wire.HookResult](t, response); !quiet(got) {
		t.Fatalf("the departed binding's receipt printed %+v", got)
	}
	p.observation(id, "own_turn")
	if n := p.count(`SELECT COUNT(*) FROM publish_attempt WHERE id = ? AND binding_id = ? AND retired_at_ms IS NULL`, id, source); n != 1 {
		t.Fatal("the original attempt lost its source binding")
	}
	wantOK(t, p.join(p.reader, move, "move-after-receipt"))
}

func TestPublishMoveWaitsForPostFenceAttemptOnSharedQueue(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	source := p.binding.ID
	wantOK(t, p.owner(CreateChannel{Name: "delivery"}, "create-delivery"))
	peer := p.newSession("claude")
	wantOK(t, p.join(peer, Join{Channel: "delivery", Alias: "peer"}, "join-peer"))
	sender := p.newSession("claude")
	wantOK(t, p.join(sender, Join{Channel: "delivery", Alias: "sender"}, "join-sender"))
	enrollAndRoute(t, p.fixture, peer, "pid-42-peer", "startup", publishTestSocket, "default", p.now.UnixMilli(), 1)
	p.binding.ID = bindingIDOf(t, p.fixture, p.principalOf(peer))
	p.proveRoute()
	p.turnEndAsk(sender, "peer", "shared-queue-work")
	id := p.reserve()
	if id == 0 || len(p.begin(id)) == 0 {
		t.Fatal("the shared queue was not occupied")
	}
	p.resolve(id, PublishSubmitted)
	refusal := p.join(p.reader, Join{Channel: "delivery", Alias: "reader", Move: true}, "move")
	wantCode(t, refusal, core.ExitRefused, core.CodeMoveNoticePending)
	if message := decode[Refusal](t, refusal).Message; !strings.Contains(message, fmt.Sprintf("notice %d ", id)) ||
		strings.Contains(message, "#checkout") {
		t.Fatalf("the peer's attempt is not in the mover's channel: %q", message)
	}
	if n := p.count(`SELECT COUNT(*) FROM member_selection WHERE binding_id = ?`, source); n != 1 {
		t.Fatal("another principal's occupied queue did not leave the source selected")
	}
	p.now = p.now.Add(time.Second)
	enrollAndRoute(t, p.fixture, p.reader, "pid-43-replacement", "resume", publishTestSocket, "default", p.now.UnixMilli(), 1)
	wantOK(t, p.join(p.reader, Join{Channel: "delivery", Alias: "reader", Move: true}, "move-from-replacement"))
	if n := p.count(`SELECT COUNT(*) FROM publish_attempt WHERE id = ? AND observed_at_ms IS NULL AND retired_at_ms IS NULL`, id); n != 1 {
		t.Fatal("the move changed the peer's unresolved attempt")
	}
}

func TestPublishMoveUsesCodexThreadLane(t *testing.T) {
	useCodexSender(t)
	f, writer := adapterChannel(t)
	f.policy("codex", true, 1)
	held, heldID := codexMember(t, f, "held")
	moving, movingID := codexMember(t, f, "moving")
	codexEvidence(t, f, "0.171.0")
	server := publish.Queue{PID: codexServer.PID, StartMS: codexServer.Start}
	wantOK(t, f.owner(CreateChannel{Name: "delivery"}, "create-delivery"))
	prove := func(proof PublishProof, queue publish.Queue) bool {
		return inTx(t, f, func(ctx context.Context, tx store.Tx) (bool, error) {
			return ConfirmPublishProof(ctx, tx, proof, publish.Proven{Queue: queue, StartedAtMS: proof.Probe.StartedHookAtMS})
		})
	}
	var oldProof PublishProof
	for _, proof := range inTx(t, f, func(ctx context.Context, tx store.Tx) ([]PublishProof, error) {
		return UnprovenPublishRoutes(ctx, tx, f.now)
	}) {
		if !prove(proof, server) {
			t.Fatalf("could not prove source route %+v", proof)
		}
		if proof.Binding.ID == movingID {
			oldProof = proof
		}
	}
	if oldProof.Binding.ID == 0 {
		t.Fatal("the moving thread had no source proof")
	}
	inTx(t, f, func(ctx context.Context, tx store.Tx) (struct{}, error) {
		return struct{}{}, ActivatePublishRoutes(ctx, tx, f.now)
	})
	f.turnEndAsk(writer, "held", "held-work")
	gen, _, _, _, _, _, _ := routeState(t, f, heldID)
	if !inTx(t, f, func(ctx context.Context, tx store.Tx) (bool, error) {
		return ReservePublish(ctx, tx, PublishBinding{ID: heldID, Generation: gen}, f.now)
	}) {
		t.Fatal("failed to reserve the held thread")
	}
	attempt := int64(f.count(`SELECT id FROM publish_attempt WHERE binding_id = ? AND lane = ?`, heldID, "thread:codex:"+held.key+":"+codexHome))
	ids := inTx(t, f, func(ctx context.Context, tx store.Tx) ([]int64, error) {
		_, ids, err := BeginPublish(ctx, tx, attempt, f.now)
		return ids, err
	})
	if len(ids) != 1 {
		t.Fatalf("held thread did not cross its final fence: %v", ids)
	}
	wantCode(t, f.join(held, Join{Channel: "delivery", Alias: "held", Move: true}, "held-move"), core.ExitRefused, core.CodeMoveNoticePending)
	wantOK(t, f.join(moving, Join{Channel: "delivery", Alias: "moving", Move: true}, "move-other-thread"))
	destination := bindingIDOf(t, f, f.principalOf(moving))
	if got := publishStateOf(t, f, destination); got.State != "candidate" || got.Reason != "route not proven" {
		t.Fatalf("unproven destination route: %+v", got)
	}
	proofs := inTx(t, f, func(ctx context.Context, tx store.Tx) ([]PublishProof, error) {
		return UnprovenPublishRoutes(ctx, tx, f.now)
	})
	if len(proofs) != 1 || proofs[0].Binding.ID != destination || proofs[0].Probe.Route.Host != server ||
		proofs[0].Probe.Route.ThreadID != moving.key || proofs[0].Probe.Route.Home != codexHome {
		t.Fatalf("destination did not keep its thread, named host and home: %+v", proofs)
	}
	if prove(oldProof, server) || prove(proofs[0], publish.Queue{PID: server.PID + 1, StartMS: server.StartMS}) {
		t.Fatal("a stale binding or another daemon proved the moved route")
	}
	if !prove(proofs[0], server) {
		t.Fatal("the moved thread could not prove the named daemon again")
	}
	inTx(t, f, func(ctx context.Context, tx store.Tx) (struct{}, error) {
		return struct{}{}, ActivatePublishRoutes(ctx, tx, f.now)
	})
	if got := publishStateOf(t, f, destination); got.State != "available" {
		t.Fatalf("activated destination route: %+v", got)
	}
	wantOK(t, f.run(held, Leave{}, "leave-held"))
	f.now = f.now.Add(time.Millisecond)
	cmd := prompt(SessionLaunch, "thread-notice", f.now.UnixMilli())
	cmd.Profile = codexProfile(publish.ModeDaemon)
	codex, _ := publish.Lookup("codex")
	line, _ := codex.Notice(ids)
	echoed, _ := codex.Echo(line)
	cmd.Receipt = notice.Receipt(echoed)
	principal := f.principalOf(held)
	result := f.exec(core.Caller{Role: core.RoleUnenrolled, PrincipalID: &principal}, Request{
		Command: cmd, Vendor: held.vendor, SessionKey: held.key, OpKey: hookKey(cmd),
	})
	if got := decode[wire.HookResult](t, result); !quiet(got) {
		t.Fatalf("late Codex receipt printed %+v", got)
	}
	if n := f.count(`SELECT COUNT(*) FROM publish_attempt WHERE id = ? AND binding_id = ? AND observation = 'own_turn' AND retired_at_ms IS NULL`, attempt, heldID); n != 1 {
		t.Fatal("the late Codex receipt did not settle its original source attempt")
	}
	wantOK(t, f.join(held, Join{Channel: "delivery", Alias: "held"}, "move-after-codex-receipt"))
}

func TestPublishMoveKeepsStrikeHistoryAndResumes(t *testing.T) {
	for _, resumed := range []bool{false, true} {
		t.Run(fmt.Sprintf("resumed=%t", resumed), func(t *testing.T) {
			p := newPublishFixture(t)
			p.proveRoute()
			p.notSubmitted(2)
			source := p.binding.ID
			if resumed {
				wantOK(t, p.owner(OwnerPublish{BindingID: source, Switch: "resume"}, "resume-source"))
			}
			wantOK(t, p.owner(CreateChannel{Name: "delivery"}, "create-delivery"))
			wantOK(t, p.join(p.reader, Join{Channel: "delivery", Alias: "reader", Move: true}, "move"))
			p.selectMoveBinding()
			p.attachMovedSocket("destination-socket")
			p.proveRoute()
			p.writer = p.newSession("shell")
			wantOK(t, p.join(p.writer, Join{Channel: "delivery", Alias: "writer"}, "join-writer"))
			id := p.notSubmitted(1)
			p.turnEndAsk(p.writer, "reader", "after-failure")
			if resumed {
				p.requirePublish("resume fence carried", "available", "")
				return
			}
			p.requirePublish("third post-fence failure", "suspended", "third in a row")
			if n := p.count(`SELECT COUNT(*) FROM publish_suspension WHERE binding_id = ? AND notice = ?`, p.binding.ID, id); n != 1 {
				t.Fatal("the suspension was not attributed to the destination notice")
			}
			if n := p.count(`SELECT COUNT(*) FROM publish_attempt WHERE binding_id = ? AND state = 'not_submitted'`, source); n != 2 {
				t.Fatal("the two source failures lost their attribution")
			}
		})
	}
}

func TestPublishMoveKeepsSuspensionAndOptOuts(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	p.wantReach("available at the source", p.turn(), ReachBringsLine)
	id := p.observed("absorbed")
	source := p.binding.ID
	p.mutate(`UPDATE publish_suspension SET until_ms = NULL WHERE notice = ?`, id)
	command := "%s Your user can turn notices back on with crosshall owner publish resume %d."
	p.wantReach("source prompt gets source command", p.turn(), fmt.Sprintf(command, ReachLostLine, source))
	wantOK(t, p.run(p.reader, Publish{}, "quiet-at-source"))
	wantOK(t, p.owner(OwnerPublish{BindingID: source, Switch: "off"}, "owner-off-at-source"))
	p.mutate(`UPDATE binding SET dnd = 1 WHERE id = ?`, source)
	wantOK(t, p.owner(CreateChannel{Name: "delivery"}, "create-delivery"))
	wantOK(t, p.join(p.reader, Join{Channel: "delivery", Alias: "reader", Move: true}, "move"))
	p.selectMoveBinding()
	if n := p.count(`SELECT COUNT(*) FROM binding WHERE id = ? AND dnd = 1 AND publish_quiet = 1 AND publish_owner = 0`, p.binding.ID); n != 1 {
		t.Fatal("move dropped the source opt-outs or owner stop")
	}
	p.attachMovedSocket("destination-socket")
	wantCode(t, p.owner(OwnerPublish{BindingID: source, Switch: "resume"}, "resume-historical"), core.ExitRefused, core.CodeBindingChanged)
	p.mutate(`UPDATE binding SET dnd = 0 WHERE id = ?`, p.binding.ID)
	wantOK(t, p.run(p.reader, Publish{On: true}, "unquiet"))
	wantOK(t, p.owner(OwnerPublish{BindingID: p.binding.ID, Switch: "default"}, "follow-destination-default"))
	p.writer = p.newSession("shell")
	wantOK(t, p.join(p.writer, Join{Channel: "delivery", Alias: "writer"}, "join-writer"))
	p.turnEndAsk(p.writer, "reader", "after-move")
	p.requirePublish("the source suspension still blocks the launch", "suspended", "absorbed")
	p.wantReach("destination prompt gets destination command", p.turn(), fmt.Sprintf(command, ReachLostLine, p.binding.ID))
	if n := p.count(`SELECT COUNT(*) FROM publish_suspension WHERE binding_id = ? AND notice = ?`, source, id); n != 1 {
		t.Fatal("the old suspension lost its source attribution")
	}
	wantOK(t, p.owner(OwnerPublish{BindingID: p.binding.ID, Switch: "resume"}, "resume-current"))
	p.proveRoute()
	p.turnEndAsk(p.writer, "reader", "after-resume")
	p.requirePublish("resume lifts the carried suspension", "available", "")
}
