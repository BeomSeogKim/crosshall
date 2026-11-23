package domain

import (
	"context"
	"fmt"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// codexMember is a Codex member started under the managed server, with
// tested publish evidence for its build.
func codexMember(t *testing.T, f *fixture, alias string) (session, int64) {
	t.Helper()
	s := f.newSession("codex")
	wantOK(t, f.join(s, Join{Channel: "checkout", Alias: alias}, alias+"-join"))
	f.hook(s, codexStart("startup", publish.ModeDaemon, f.now.UnixMilli()))
	if _, err := f.db.Exec(`INSERT OR IGNORE INTO capability_evidence (vendor, vendor_version, platform, mode, manifest_rev, cap, level, result_ref, checked_at_ms)
		VALUES ('codex', '0.171.0', 'darwin-arm64', 'daemon', 'codex-1', 'publish', 'tested', 'codex-thread-test', 1)`); err != nil {
		t.Fatal(err)
	}
	return s, bindingIDOf(t, f, f.principalOf(s))
}

func publishStateOf(t *testing.T, f *fixture, binding int64) store.PublishState {
	t.Helper()
	state, found, err := store.ReadPublishState(t.Context(), f.db, binding, f.now)
	if err != nil || !found {
		t.Fatalf("publish state of %d: %+v %v %v", binding, state, found, err)
	}
	return state
}

func inTx[T any](t *testing.T, f *fixture, apply func(context.Context, store.Tx) (T, error)) T {
	t.Helper()
	tx, err := f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	got, err := apply(t.Context(), tx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return got
}

// A Codex launch is never a verified one: its route publishes on the
// managed server its start named, once the transport proves that server.
// Each thread is its own lane, so two members on one server do not hold
// each other's notices (#236, 9c).
func TestCodexThreadRouteIsProvenOnItsServer(t *testing.T) {
	t.Cleanup(publish.Swap(publish.Shipped().WithTransport("codex", publish.Transport{
		Send: func(context.Context, publish.Target, string) publish.Outcome { return publish.Submitted },
	})))
	f, writer := adapterChannel(t)
	f.policy("codex", true, 1)
	_, firstBinding := codexMember(t, f, "first")
	_, secondBinding := codexMember(t, f, "second")

	if got := publishStateOf(t, f, firstBinding); got.State != "candidate" || got.Reason != "route not proven" {
		t.Fatalf("before its proof a Codex route reads %s (%s)", got.State, got.Reason)
	}
	proofs := inTx(t, f, func(ctx context.Context, tx store.Tx) ([]PublishProof, error) {
		return UnprovenPublishRoutes(ctx, tx, f.now)
	})
	if len(proofs) != 2 {
		t.Fatalf("unproven routes: %+v", proofs)
	}
	server := publish.Queue{PID: codexServer.PID, StartMS: codexServer.Start}
	for _, proof := range proofs {
		if proof.Probe.Route.Host != server || proof.Probe.Address != proof.Probe.Route.ThreadID {
			t.Fatalf("probe: %+v", proof.Probe)
		}
		started := proof.Probe.StartedHookAtMS
		for name, proven := range map[string]publish.Proven{
			"another process":        {Queue: publish.Queue{PID: server.PID + 1, StartMS: server.StartMS}, StartedAtMS: started},
			"another start":          {Queue: publish.Queue{PID: server.PID, StartMS: server.StartMS + 1}, StartedAtMS: started},
			"started after its hook": {Queue: server, StartedAtMS: started + 1},
		} {
			if ok := inTx(t, f, func(ctx context.Context, tx store.Tx) (bool, error) {
				return ConfirmPublishProof(ctx, tx, proof, proven)
			}); ok {
				t.Fatalf("%s was recorded as the server", name)
			}
		}
		if ok := inTx(t, f, func(ctx context.Context, tx store.Tx) (bool, error) {
			return ConfirmPublishProof(ctx, tx, proof, publish.Proven{Queue: server, StartedAtMS: started})
		}); !ok {
			t.Fatalf("the named server was not recorded for %+v", proof)
		}
	}
	inTx(t, f, func(ctx context.Context, tx store.Tx) (struct{}, error) {
		return struct{}{}, ActivatePublishRoutes(ctx, tx, f.now)
	})
	for _, binding := range []int64{firstBinding, secondBinding} {
		if got := publishStateOf(t, f, binding); got.State != "available" {
			t.Fatalf("proven Codex route %d reads %s (%s)", binding, got.State, got.Reason)
		}
	}

	f.turnEndAsk(writer, "first", "to-first")
	f.turnEndAsk(writer, "second", "to-second")
	for _, binding := range []int64{firstBinding, secondBinding} {
		b := PublishBinding{ID: binding}
		b.Generation, _, _, _, _, _, _ = routeState(t, f, binding)
		if !inTx(t, f, func(ctx context.Context, tx store.Tx) (bool, error) { return ReservePublish(ctx, tx, b, f.now) }) {
			t.Fatalf("binding %d did not reserve: one server's other thread holds no lane of its own", binding)
		}
		if got := publishStateOf(t, f, binding); got.State != "held" {
			t.Fatalf("binding %d after its notice reads %s", binding, got.State)
		}
	}
	rows, err := f.db.Query(`SELECT a.lane, r.thread_id FROM publish_attempt a JOIN publish_route r ON r.binding_id = a.binding_id ORDER BY a.id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var lane, thread string
		if err := rows.Scan(&lane, &thread); err != nil {
			t.Fatal(err)
		}
		if lane != fmt.Sprintf("thread:codex:%s", thread) {
			t.Fatalf("lane %q for thread %q", lane, thread)
		}
	}

	// A route whose start named no server, as one registered before
	// servers were named, cannot be proven or selected.
	if _, err := f.db.Exec(`UPDATE publish_route SET host_pid = NULL, host_start = NULL WHERE binding_id = ?`, firstBinding); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE publish_attempt SET observed_at_ms = 1, observation = 'own_turn'`); err != nil {
		t.Fatal(err)
	}
	if got := publishStateOf(t, f, firstBinding); got.State != "candidate" || got.Reason != publish.CodexDaemonUnverified {
		t.Fatalf("a route without its server reads %s (%s)", got.State, got.Reason)
	}
	f.turnEndAsk(writer, "first", "to-first-again")
	if inTx(t, f, func(ctx context.Context, tx store.Tx) (bool, error) {
		return ReservePublish(ctx, tx, PublishBinding{ID: firstBinding, Generation: 1}, f.now)
	}) {
		t.Fatal("a route without its server reserved a notice")
	}
}
