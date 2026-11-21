package domain

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

func newProbationFixture(t *testing.T) *publishFixture {
	t.Helper()
	p := newPublishFixture(t)
	p.mutate(`UPDATE capability_evidence SET vendor_version = '2.1.326' WHERE cap = 'publish'`)
	p.proveRoute()
	return p
}

func probationNotice(p *publishFixture, binding PublishBinding, alias, key string) (int64, []int64) {
	p.t.Helper()
	ask := p.turnEndAsk(p.writer, alias, key)
	var id int64
	var ids []int64
	p.update(func(ctx context.Context, tx store.Tx) error {
		ok, err := ReservePublish(ctx, tx, binding, p.now)
		if err != nil || !ok {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT MAX(id) FROM publish_attempt WHERE binding_id = ?`, binding.ID).Scan(&id); err != nil {
			return err
		}
		_, ids, err = BeginPublish(ctx, tx, id, p.now)
		return err
	})
	if id == 0 || !slices.Equal(ids, []int64{ask}) {
		p.t.Fatalf("binding %d started attempt %d with %v, want [%d]", binding.ID, id, ids, ask)
	}
	p.resolve(id, PublishSubmitted)
	return id, ids
}

func probationBuild(p *publishFixture, confirmed int, stopRef int64, reason string, lifted bool) int64 {
	p.t.Helper()
	var count int
	var stopped, lift, ref sql.NullInt64
	var stopReason sql.NullString
	err := p.db.QueryRow(`SELECT confirmed, stopped_at_ms, stop_ref, stop_reason, lifted_at_ms FROM capability_build
		WHERE (vendor, vendor_version, platform, mode, manifest_rev, cap) = (?, ?, ?, ?, ?, 'publish')`,
		testProfile.Adapter, testProfile.VendorVersion, testProfile.Platform, testProfile.Mode, testProfile.ManifestRev).
		Scan(&count, &stopped, &ref, &stopReason, &lift)
	if err != nil || count != confirmed || ref.Int64 != stopRef || stopReason.String != reason || lift.Valid != lifted ||
		stopped.Valid != (stopRef != 0) || (lifted && lift.Int64 < stopped.Int64) {
		p.t.Fatalf("build confirmations %d, stop %v/%v at %v, lift %v, error %v; want %d, %d/%q, lifted %t",
			count, ref, stopReason, stopped, lift, err, confirmed, stopRef, reason, lifted)
	}
	return stopped.Int64
}

func TestPublishProbationCountsUniqueReceiptsUpToThree(t *testing.T) {
	p := newProbationFixture(t)
	for i := 1; i <= store.PublishBuildConfirmations+1; i++ {
		turn := fmt.Sprintf("own-turn-%d", i)
		id, ids := probationNotice(p, p.binding, "reader", turn)
		cmd := p.promptReceipt(turn, publishTestLaunch, p.now.UnixMilli()+10, ids)
		p.hook(p.reader, cmd)
		p.hook(p.reader, cmd)
		p.observation(id, "own_turn")
		probationBuild(p, min(i, store.PublishBuildConfirmations), 0, "", false)
		p.hook(p.reader, stop(publishTestLaunch, turn, p.now.UnixMilli()+10))
	}
	turn := "already-open"
	p.hook(p.reader, prompt(publishTestLaunch, turn, p.now.UnixMilli()+10))
	id, ids := probationNotice(p, p.binding, "reader", turn)
	p.hook(p.reader, p.promptReceipt(turn, publishTestLaunch, p.now.UnixMilli()+10, ids))
	p.observation(id, "absorbed")
	probationBuild(p, store.PublishBuildConfirmations, 0, "", false)
	if n := p.count(`SELECT COUNT(*) FROM publish_suspension WHERE binding_id = ? AND notice = ? AND reason = 'absorbed'`, p.binding.ID, id); n != 1 {
		t.Fatal("a completed build lost its launch suspension")
	}
}

func TestPublishProbationSharesTheFirstStopAndResumesThroughAnotherBinding(t *testing.T) {
	p := newProbationFixture(t)
	peer := p.newSession("claude")
	wantOK(t, p.join(peer, Join{Channel: "checkout", Alias: "peer"}, "peer-join"))
	const peerLaunch = "pid-43-peer"
	enrollAndRoute(t, p.fixture, peer, peerLaunch, "startup", "/tmp/cc-socks/43.sock", "default", p.now.UnixMilli(), 1)
	peerBinding := PublishBinding{ID: bindingIDOf(t, p.fixture, p.principalOf(peer))}
	peerBinding.Generation, _, _, _, _, _, _ = routeState(t, p.fixture, peerBinding.ID)
	p.update(func(ctx context.Context, tx store.Tx) error {
		return ConfirmPublishRoute(ctx, tx, peerBinding, PublishQueue{PID: 43, StartMS: p.queue.StartMS})
	})
	p.activate()

	confirm, ids := probationNotice(p, peerBinding, "peer", "confirm")
	p.hook(peer, p.promptReceipt("confirm", peerLaunch, p.now.UnixMilli()+10, ids))
	p.observation(confirm, "own_turn")
	probationBuild(p, 1, 0, "", false)
	p.hook(peer, stop(peerLaunch, "confirm", p.now.UnixMilli()+10))
	p.hook(p.reader, prompt(publishTestLaunch, "reader-open", p.now.UnixMilli()+10))
	p.hook(peer, prompt(peerLaunch, "peer-open", p.now.UnixMilli()+10))
	first, firstIDs := probationNotice(p, p.binding, "reader", "stop-first")
	later, laterIDs := probationNotice(p, peerBinding, "peer", "stop-later")
	p.hook(p.reader, p.promptReceipt("reader-open", publishTestLaunch, p.now.UnixMilli()+10, firstIDs))
	firstAt := probationBuild(p, 1, first, "absorbed", false)
	state, found, err := store.ReadPublishState(t.Context(), p.db, peerBinding.ID, p.now)
	if err != nil || !found || state.State != "suspended" || state.Notice != first {
		t.Fatalf("peer did not show the shared stop: %+v, found %t, error %v", state, found, err)
	}
	p.hook(peer, p.promptReceipt("peer-open", peerLaunch, p.now.UnixMilli()+10, laterIDs))
	p.observation(later, "absorbed")
	if after := probationBuild(p, 1, first, "absorbed", false); after != firstAt {
		t.Fatal("an in-flight receipt moved the shared stop")
	}
	if n := p.count(`SELECT COUNT(*) FROM publish_suspension WHERE notice IN (?, ?) AND reason = 'absorbed'`, first, later); n != 2 {
		t.Fatal("the two launches did not retain independent suspensions")
	}
	p.mutate(`INSERT INTO capability_build (vendor, vendor_version, platform, mode, manifest_rev, cap, confirmed, stopped_at_ms, stop_reason, stop_ref)
		SELECT vendor, '2.1.328', platform, mode, manifest_rev, cap, confirmed, stopped_at_ms, stop_reason, stop_ref FROM capability_build`)

	cmd := OwnerPublish{BindingID: peerBinding.ID, Switch: "resume"}
	if resumed := decode[PublishSwitchResult](t, p.owner(cmd, "resume-peer")); !resumed.BuildStopLifted {
		t.Fatalf("resume through peer did not report a shared lift: %+v", resumed)
	}
	probationBuild(p, 1, first, "absorbed", true)
	if n := p.count(`SELECT COUNT(*) FROM capability_build WHERE vendor_version = '2.1.328' AND lifted_at_ms IS NULL`); n != 1 {
		t.Fatal("resume lifted a stop for another build")
	}
	if resumed := decode[PublishSwitchResult](t, p.owner(cmd, "resume-peer-again")); resumed.BuildStopLifted {
		t.Fatalf("resume reported a second shared lift: %+v", resumed)
	}
	p.now = p.now.Add(time.Millisecond)
	restopped, restoppedIDs := probationNotice(p, peerBinding, "peer", "stop-again")
	p.hook(peer, p.promptReceipt("peer-open", peerLaunch, p.now.UnixMilli()+10, restoppedIDs))
	probationBuild(p, 1, restopped, "absorbed", false)
	if replay := decode[PublishSwitchResult](t, p.owner(cmd, "resume-peer")); !replay.BuildStopLifted {
		t.Fatalf("original resume result did not replay: %+v", replay)
	}
	probationBuild(p, 1, restopped, "absorbed", false)
}

func TestPublishProbationDoesNotTrackExactlyTestedBuilds(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	turn := "open-exact"
	p.hook(p.reader, prompt(publishTestLaunch, turn, p.now.UnixMilli()+10))
	id, ids := probationNotice(p, p.binding, "reader", turn)
	p.hook(p.reader, p.promptReceipt(turn, publishTestLaunch, p.now.UnixMilli()+10, ids))
	p.observation(id, "absorbed")
	if n := p.count(`SELECT COUNT(*) FROM capability_build`); n != 0 {
		t.Fatalf("exact tested receipt created %d build records", n)
	}
	if n := p.count(`SELECT COUNT(*) FROM publish_suspension WHERE notice = ? AND reason = 'absorbed'`, id); n != 1 {
		t.Fatal("exact tested build lost its launch suspension")
	}
}

func TestPublishProbationRequiresAnAcceptedNewerExactTestToLift(t *testing.T) {
	p := newProbationFixture(t)
	turn := "open"
	p.hook(p.reader, prompt(publishTestLaunch, turn, p.now.UnixMilli()+10))
	p.hook(p.reader, event(publishTestLaunch, FactApprovalRequested, "approval", "another-turn", p.now.UnixMilli()+10, false))
	id, ids := probationNotice(p, p.binding, "reader", "unknown")
	p.hook(p.reader, p.promptReceipt("notice", publishTestLaunch, p.now.UnixMilli()+10, ids))
	p.observation(id, "unknown")
	stopped := probationBuild(p, 0, id, "unknown", false)
	evidence := AdapterEvidence{Vendor: testProfile.Adapter, VendorVersion: testProfile.VendorVersion, Platform: testProfile.Platform,
		Mode: testProfile.Mode, ManifestRev: testProfile.ManifestRev, RunAtMS: stopped, ResultRef: "sha256:probation",
		Results: []EvidenceResult{{Cap: "publish", Level: "tested", Check: "notice opened its own turn"}}}
	importRun := func(cmd AdapterEvidence, key string, rows int) {
		t.Helper()
		if result := decode[AdapterEvidenceResult](t, p.owner(cmd, key)); result.Rows != rows {
			t.Fatalf("%s recorded %d rows, want %d", key, result.Rows, rows)
		}
	}
	importRun(evidence, "same-time", 1)
	probationBuild(p, 0, id, "unknown", false)
	other := evidence
	other.VendorVersion, other.RunAtMS = "2.1.328", stopped+1
	importRun(other, "other-build", 1)
	probationBuild(p, 0, id, "unknown", false)
	negative := evidence
	negative.RunAtMS += 2
	negative.Results = []EvidenceResult{{Cap: "publish", Level: "candidate"}}
	importRun(negative, "newer-untested", 1)
	evidence.RunAtMS++
	importRun(evidence, "stale-tested", 0)
	probationBuild(p, 0, id, "unknown", false)
	evidence.RunAtMS += 2
	importRun(evidence, "newer-tested", 1)
	probationBuild(p, 0, id, "unknown", true)
	if n := p.count(`SELECT COUNT(*) FROM publish_suspension WHERE notice = ? AND reason = 'unknown'`, id); n != 1 {
		t.Fatal("accepted evidence removed the separate launch suspension")
	}
}
