package domain

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// useCodexSender gives Codex a sender, as 9c-2 will, for one test.
func useCodexSender(t *testing.T) {
	t.Helper()
	t.Cleanup(publish.Swap(publish.Shipped().WithTransport("codex", publish.Transport{
		Send: func(context.Context, publish.Target, string) publish.Outcome { return publish.Submitted },
	})))
}

// codexEvidence records tested publish evidence for a Codex version.
func codexEvidence(t *testing.T, f *fixture, version string) {
	t.Helper()
	if _, err := f.db.Exec(`INSERT OR IGNORE INTO capability_evidence (vendor, vendor_version, platform, mode, manifest_rev, cap, level, result_ref, checked_at_ms)
		VALUES ('codex', ?, 'darwin-arm64', 'daemon', 'codex-1', 'publish', 'tested', 'codex-thread-test', 1)`, version); err != nil {
		t.Fatal(err)
	}
}

// codexMember is a Codex member started under the managed server.
func codexMember(t *testing.T, f *fixture, alias string) (session, int64) {
	t.Helper()
	s := f.newSession("codex")
	wantOK(t, f.join(s, Join{Channel: "checkout", Alias: alias}, alias+"-join"))
	f.hook(s, codexStart("startup", publish.ModeDaemon, f.now.UnixMilli()))
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

// proveCodexRoutes records the named server as the proof of every route
// that waits for one, as the Codex transport would, and activates them.
func proveCodexRoutes(t *testing.T, f *fixture) {
	t.Helper()
	proofs := inTx(t, f, func(ctx context.Context, tx store.Tx) ([]PublishProof, error) {
		return UnprovenPublishRoutes(ctx, tx, f.now)
	})
	for _, proof := range proofs {
		proven := publish.Proven{Queue: proof.Probe.Route.Host, StartedAtMS: proof.Probe.StartedHookAtMS}
		if !inTx(t, f, func(ctx context.Context, tx store.Tx) (bool, error) {
			return ConfirmPublishProof(ctx, tx, proof, proven)
		}) {
			t.Fatalf("the named server was not recorded for %+v", proof)
		}
	}
	inTx(t, f, func(ctx context.Context, tx store.Tx) (struct{}, error) {
		return struct{}{}, ActivatePublishRoutes(ctx, tx, f.now)
	})
}

func codexReserve(t *testing.T, f *fixture, binding int64) bool {
	t.Helper()
	b := PublishBinding{ID: binding}
	b.Generation, _, _, _, _, _, _ = routeState(t, f, binding)
	return inTx(t, f, func(ctx context.Context, tx store.Tx) (bool, error) { return ReservePublish(ctx, tx, b, f.now) })
}

func openLanes(t *testing.T, f *fixture) map[int64]string {
	t.Helper()
	rows, err := f.db.Query(`SELECT binding_id, lane FROM publish_attempt WHERE observed_at_ms IS NULL AND retired_at_ms IS NULL`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	lanes := map[int64]string{}
	for rows.Next() {
		var binding int64
		var lane string
		if err := rows.Scan(&binding, &lane); err != nil {
			t.Fatal(err)
		}
		lanes[binding] = lane
	}
	return lanes
}

// A Codex launch is never a verified one: its route publishes on the
// managed server its start named, once the transport proves that server.
// Each thread is its own lane, so two members on one server do not hold
// each other's notices (#236, 9c).
func TestCodexThreadRouteIsProvenOnItsServer(t *testing.T) {
	useCodexSender(t)
	f, writer := adapterChannel(t)
	f.policy("codex", true, 1)
	_, firstBinding := codexMember(t, f, "first")
	_, secondBinding := codexMember(t, f, "second")
	codexEvidence(t, f, "0.171.0")

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
		if proof.Probe.Route.Host != server || proof.Probe.Route.Home != codexHome || proof.Probe.Address != proof.Probe.Route.ThreadID {
			t.Fatalf("probe: %+v", proof.Probe)
		}
		started := proof.Probe.StartedHookAtMS
		for name, proven := range map[string]publish.Proven{
			"another process":        {Queue: publish.Queue{PID: server.PID + 1, StartMS: server.StartMS}, StartedAtMS: started},
			"another start":          {Queue: publish.Queue{PID: server.PID, StartMS: server.StartMS + 1}, StartedAtMS: started},
			"started after its hook": {Queue: server, StartedAtMS: started + 1},
		} {
			if inTx(t, f, func(ctx context.Context, tx store.Tx) (bool, error) {
				return ConfirmPublishProof(ctx, tx, proof, proven)
			}) {
				t.Fatalf("%s was recorded as the server", name)
			}
		}
	}
	proveCodexRoutes(t, f)
	for _, binding := range []int64{firstBinding, secondBinding} {
		if got := publishStateOf(t, f, binding); got.State != "available" {
			t.Fatalf("proven Codex route %d reads %s (%s)", binding, got.State, got.Reason)
		}
	}

	f.turnEndAsk(writer, "first", "to-first")
	f.turnEndAsk(writer, "second", "to-second")
	for _, binding := range []int64{firstBinding, secondBinding} {
		if !codexReserve(t, f, binding) {
			t.Fatalf("binding %d did not reserve: one server's other thread holds no lane of its own", binding)
		}
		if got := publishStateOf(t, f, binding); got.State != "held" {
			t.Fatalf("binding %d after its notice reads %s", binding, got.State)
		}
	}
	for binding, lane := range openLanes(t, f) {
		var thread string
		if err := f.db.QueryRow(`SELECT thread_id FROM publish_route WHERE binding_id = ?`, binding).Scan(&thread); err != nil {
			t.Fatal(err)
		}
		if lane != fmt.Sprintf("thread:codex:%s:%s", thread, codexHome) {
			t.Fatalf("lane %q for thread %q", lane, thread)
		}
	}

	// A route whose start named no server, as one registered before
	// servers were named, cannot be proven or selected.
	if _, err := f.db.Exec(`UPDATE publish_route SET host_pid = NULL, host_start = NULL, vendor_home = NULL WHERE binding_id = ?`, firstBinding); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`UPDATE publish_attempt SET observed_at_ms = 1, observation = 'own_turn'`); err != nil {
		t.Fatal(err)
	}
	if got := publishStateOf(t, f, firstBinding); got.State != "candidate" || got.Reason != publish.CodexDaemonUnverified {
		t.Fatalf("a route without its server reads %s (%s)", got.State, got.Reason)
	}
	f.turnEndAsk(writer, "first", "to-first-again")
	if codexReserve(t, f, firstBinding) {
		t.Fatal("a route without its server reserved a notice")
	}
}

// Each Codex home numbers its own threads, so its lane is the thread in
// that home: the same thread ID in two homes holds two lanes, the same
// thread in one home one lane, also after its server restarts (#236, 9c).
func TestCodexThreadLaneIsTheThreadInItsHome(t *testing.T) {
	useCodexSender(t)
	f, writer := adapterChannel(t)
	f.policy("codex", true, 1)
	first, firstBinding := codexMember(t, f, "first")
	_, secondBinding := codexMember(t, f, "second")
	codexEvidence(t, f, "0.171.0")
	proveCodexRoutes(t, f)
	var thread string
	if err := f.db.QueryRow(`SELECT thread_id FROM publish_route WHERE binding_id = ?`, firstBinding).Scan(&thread); err != nil {
		t.Fatal(err)
	}
	sameThread := func(home string) {
		t.Helper()
		if _, err := f.db.Exec(`UPDATE publish_route SET thread_id = ?, vendor_home = ? WHERE binding_id = ?`, thread, home, secondBinding); err != nil {
			t.Fatal(err)
		}
	}
	observeAll := func() {
		t.Helper()
		if _, err := f.db.Exec(`UPDATE publish_attempt SET observed_at_ms = 1, observation = 'own_turn' WHERE observed_at_ms IS NULL`); err != nil {
			t.Fatal(err)
		}
	}

	sameThread("/home/other/.codex")
	f.turnEndAsk(writer, "first", "first-1")
	f.turnEndAsk(writer, "second", "second-1")
	if !codexReserve(t, f, firstBinding) || !codexReserve(t, f, secondBinding) {
		t.Fatal("one thread ID in two homes shared a lane")
	}
	observeAll()

	sameThread(codexHome)
	f.turnEndAsk(writer, "first", "first-2")
	f.turnEndAsk(writer, "second", "second-2")
	if !codexReserve(t, f, firstBinding) {
		t.Fatal("the first notice did not reserve")
	}
	held := openLanes(t, f)[firstBinding]
	if codexReserve(t, f, secondBinding) || publishStateOf(t, f, secondBinding).State != "held" {
		t.Fatal("one thread in one home took a second lane")
	}
	observeAll()

	// The server restarts: the session's next start names the new one, in
	// the same home. Its thread keeps its lane.
	restart := codexStart("resume", publish.ModeDaemon, f.now.UnixMilli()+1_000)
	restart.Host = &wire.Host{PID: codexServer.PID + 1, Start: codexServer.Start + 1}
	f.hook(first, restart)
	proveCodexRoutes(t, f)
	f.turnEndAsk(writer, "first", "first-3")
	if !codexReserve(t, f, firstBinding) || openLanes(t, f)[firstBinding] != held {
		t.Fatalf("after a server restart the thread's lane is %q, want %q", openLanes(t, f)[firstBinding], held)
	}
}

func codexPrompt(turn string, at int64) HookEvent {
	cmd := prompt(SessionLaunch, turn, at)
	cmd.Profile = codexProfile(publish.ModeDaemon)
	return cmd
}

// A Codex build that inherits tested evidence from another version runs on
// trial like any other: its receipts confirm it, an absorbed one stops it
// for every member on it, and the owner's resume lifts that stop, although
// no Codex launch is a verified one (#236, 9c).
func TestCodexInheritedBuildIsConfirmedStoppedAndResumed(t *testing.T) {
	useCodexSender(t)
	f, writer := adapterChannel(t)
	f.policy("codex", true, 1)
	first, firstBinding := codexMember(t, f, "first")
	_, secondBinding := codexMember(t, f, "second")
	codexEvidence(t, f, "0.170.0")
	proveCodexRoutes(t, f)
	if got := publishStateOf(t, f, firstBinding); got.State != "available" || !strings.Contains(got.Reason, "trusted from codex-1 evidence, 0 of 3") {
		t.Fatalf("an inherited Codex build reads %s (%s)", got.State, got.Reason)
	}
	p := &publishFixture{fixture: f, writer: writer}
	build := func() (confirmed int, stopRef int64) {
		t.Helper()
		err := f.db.QueryRow(`SELECT confirmed, COALESCE(stop_ref, 0) FROM capability_build
			WHERE (vendor, vendor_version, platform, mode, manifest_rev, cap) = ('codex', '0.171.0', 'darwin-arm64', 'daemon', 'codex-1', 'publish')`).
			Scan(&confirmed, &stopRef)
		if err != nil {
			t.Fatalf("build record: %v", err)
		}
		return confirmed, stopRef
	}
	at := f.now.UnixMilli()
	binding := func(id int64) PublishBinding {
		b := PublishBinding{ID: id}
		b.Generation, _, _, _, _, _, _ = routeState(t, f, id)
		return b
	}

	confirm, ids := probationNotice(p, binding(firstBinding), "first", "confirm")
	f.hook(first, codexReceipt("confirm", at+10, ids))
	p.observation(confirm, "own_turn")
	if confirmed, stopRef := build(); confirmed != 1 || stopRef != 0 {
		t.Fatalf("after an own-turn receipt: confirmed %d, stopped by %d", confirmed, stopRef)
	}

	f.hook(first, codexPrompt("open", at+20))
	absorbed, ids := probationNotice(p, binding(firstBinding), "first", "absorbed")
	f.hook(first, codexReceipt("open", at+30, ids))
	p.observation(absorbed, "absorbed")
	if _, stopRef := build(); stopRef != absorbed {
		t.Fatalf("an absorbed receipt did not stop the build: stopped by %d, want %d", stopRef, absorbed)
	}
	if got := publishStateOf(t, f, secondBinding); got.State != "suspended" || got.Notice != absorbed {
		t.Fatalf("another member on the stopped build reads %s (%s)", got.State, got.Reason)
	}

	resumed := decode[PublishSwitchResult](t, f.owner(OwnerPublish{BindingID: firstBinding, Switch: "resume"}, "resume-first"))
	if !resumed.BuildStopLifted {
		t.Fatalf("resume did not lift the build stop: %+v", resumed)
	}
	if got := publishStateOf(t, f, secondBinding); got.State != "available" {
		t.Fatalf("after the resume another member reads %s (%s)", got.State, got.Reason)
	}
}
