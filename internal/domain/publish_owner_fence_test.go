package domain

import (
	"context"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

func TestPublishMoveAfterOwnerDepartureRequiresNewLaunch(t *testing.T) {
	useCodexSender(t)
	f, _ := adapterChannel(t)
	f.policy("codex", true, 1)
	member, source := codexMember(t, f, "reader")
	codexEvidence(t, f, "0.171.0")
	proveCodexRoutes(t, f)
	wantOK(t, f.owner(CreateChannel{Name: "delivery"}, "create-delivery"))
	seen, err := departureToken(t.Context(), f.db, source, f.principalOf(member))
	if err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(time.Second)
	wantOK(t, f.owner(OwnerDepart{BindingID: source, Seen: seen}, "depart"))
	f.now = f.now.Add(time.Second)
	wantOK(t, f.join(member, Join{Channel: "delivery", Alias: "reader"}, "join-delivery"))
	destination := bindingIDOf(t, f, f.principalOf(member))
	if n := f.count(`SELECT COUNT(*) FROM publish_route WHERE binding_id = ? AND state = 'active'`, destination); n != 0 {
		t.Fatal("the manual join registered the owner-fenced launch")
	}
	proofs := inTx(t, f, func(ctx context.Context, tx store.Tx) ([]PublishProof, error) {
		return UnprovenPublishRoutes(ctx, tx, f.now)
	})
	if len(proofs) != 0 {
		t.Fatalf("proof requested before a new start: %+v", proofs)
	}

	f.now = f.now.Add(time.Second)
	f.hook(member, codexStart("resume", publish.ModeDaemon, f.now.UnixMilli()))
	proveCodexRoutes(t, f)
	if state := publishStateOf(t, f, destination); state.State != "available" {
		t.Fatalf("fresh launch: %+v", state)
	}
}

func TestPublishRouteCallbackAfterMoveRequiresNewLaunch(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	wantOK(t, p.owner(CreateChannel{Name: "delivery"}, "create-delivery"))
	seen, err := departureToken(t.Context(), p.db, p.binding.ID, p.principalOf(p.reader))
	if err != nil {
		t.Fatal(err)
	}
	p.now = p.now.Add(time.Second)
	wantOK(t, p.owner(OwnerDepart{BindingID: p.binding.ID, Seen: seen}, "depart"))
	p.now = p.now.Add(time.Second)
	wantOK(t, p.join(p.reader, Join{Channel: "delivery", Alias: "reader"}, "join-delivery"))
	destination := bindingIDOf(t, p.fixture, p.principalOf(p.reader))

	p.now = p.now.Add(time.Second)
	wantOK(t, p.exec(p.caller(p.reader), Request{
		Command: claudeRoute(publishTestLaunch, "old-launch-socket", publishTestSocket, "default", p.now.UnixMilli(), 1),
		Vendor:  p.reader.vendor, SessionKey: p.reader.key, OpKey: "old-launch-socket",
	}))
	if n := p.count(`SELECT COUNT(*) FROM publish_route WHERE binding_id = ? AND state = 'active'`, destination); n != 0 {
		t.Fatal("a newer route callback registered the owner-fenced launch")
	}
}

func TestPublishProofAndEligibilityRejectOwnerFencedLaunch(t *testing.T) {
	useCodexSender(t)
	f, writer := adapterChannel(t)
	f.policy("codex", true, 1)
	_, binding := codexMember(t, f, "reader")
	codexEvidence(t, f, "0.171.0")
	proofs := inTx(t, f, func(ctx context.Context, tx store.Tx) ([]PublishProof, error) {
		return UnprovenPublishRoutes(ctx, tx, f.now)
	})
	if len(proofs) != 1 {
		t.Fatalf("expected one proof request, got %+v", proofs)
	}
	proof := proofs[0]
	if _, err := f.db.Exec(`UPDATE binding SET owner_hook_fence_ms = ? WHERE id = ?`, proof.Probe.StartedHookAtMS, binding); err != nil {
		t.Fatal(err)
	}
	if inTx(t, f, func(ctx context.Context, tx store.Tx) (bool, error) {
		return ConfirmPublishProof(ctx, tx, proof, publish.Proven{
			Queue: proof.Probe.Route.Host, StartedAtMS: proof.Probe.StartedHookAtMS,
		})
	}) {
		t.Error("the in-flight proof crossed the owner fence")
	}
	queue := proof.Probe.Route.Host
	inTx(t, f, func(ctx context.Context, tx store.Tx) (struct{}, error) {
		return struct{}{}, ConfirmPublishRoute(ctx, tx, proof.Binding, queue)
	})
	if n := f.count(`SELECT COUNT(*) FROM publish_route_proof WHERE binding_id = ?`, binding); n != 0 {
		t.Error("the owner-fenced launch recorded new proof")
	}
	if _, err := f.db.Exec(`INSERT INTO publish_route_proof (binding_id, generation, process_pid, process_start_ms, activated_at_ms)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT(binding_id) DO UPDATE SET activated_at_ms = excluded.activated_at_ms`,
		binding, proof.Binding.Generation, queue.PID, queue.StartMS, f.now.UnixMilli()); err != nil {
		t.Fatal(err)
	}
	f.turnEndAsk(writer, "reader", "after-fence")
	if state := publishStateOf(t, f, binding); PublishReaches(state) {
		t.Errorf("stale proof made the fenced route available: %+v", state)
	}
	if inTx(t, f, func(ctx context.Context, tx store.Tx) (bool, error) {
		return ReservePublish(ctx, tx, proof.Binding, f.now)
	}) {
		t.Error("stale proof let the fenced route reserve a notice")
	}
}
