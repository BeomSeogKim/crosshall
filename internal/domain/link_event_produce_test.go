package domain

import (
	"slices"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/link"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func TestLinkOfferReplacementKeepsHistoricalWrittenReceipts(t *testing.T) {
	f := newFixture(t)
	ctx := t.Context()
	wantOK(t, f.owner(CreateChannel{Name: "dev"}, "channel"))
	execArgs(f, `INSERT INTO adapter_policy(adapter, enabled, reminder, epoch, changed_at_ms) VALUES ('claude', 1, 0, 7, 1)`)
	device := decodeAs[LinkEnrollResult](t, f.owner(LinkEnroll{Name: "spoke", Secret: secretHex(1)}, "enroll")).Device
	member := link.Member{Vendor: "claude", Key: "remote-session"}
	wantOK(t, f.exec(core.Caller{Role: core.RoleUnenrolled}, Request{
		OpKey: "remote-join", Vendor: member.Vendor, Command: Join{Channel: "dev", Alias: "remote"},
		Enroll: enrollDeviceKey(LinkCall{Device: device, CredentialGen: 1, Vendor: member.Vendor, Key: member.Key, LeafGeneration: "leaf", LeafEpoch: 1}, f.now),
	}))
	sender := f.newSession("codex")
	wantOK(t, f.join(sender, Join{Channel: "dev", Alias: "sender"}, "sender-join"))
	request := decodeAs[AskResult](t, f.exec(f.caller(sender), Request{OpKey: "ask", Command: Ask{To: []string{"remote"}, Body: "A request"}}))
	message := decodeAs[NoteResult](t, f.exec(f.caller(sender), Request{OpKey: "note", Command: Note{To: []string{"remote"}, Body: "A note"}}))

	update := func(apply func(store.Tx) error) {
		t.Helper()
		tx, err := f.db.BeginTx(ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		if err := apply(tx); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	update(func(tx store.Tx) error {
		scope, err := readLinkOfferScope(ctx, tx, device, member)
		if err != nil {
			return err
		}
		p := wire.HookProfile{Adapter: "claude", ManifestRev: "claude-3", VendorVersion: "2.1.331", Platform: "darwin-arm64", Mode: "interactive"}
		if err := registerLaunch(ctx, tx, scope.principal, p, "projection", false, "", ms(f.now)); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO link_event_launch
			(principal_id, device_id, credential_gen, boot, leaf_generation, binding_id, membership_generation, route_epoch,
			 launch_ref, projection_ref, policy_epoch, verified, hosted)
			VALUES (?, ?, ?, 'boot', ?, ?, ?, ?, 'launch', 'projection', 7, 1, 1)`,
			scope.principal, device, scope.credential, scope.LeafGeneration, scope.Membership.BindingID,
			scope.Membership.Generation, scope.RouteEpoch)
		return err
	})
	snapshot := func() []link.SnapshotMember {
		t.Helper()
		var members []link.SnapshotMember
		update(func(tx store.Tx) error {
			if err := refreshLinkEvents(ctx, tx, device, f.now); err != nil {
				return err
			}
			var err error
			members, err = snapshotLinkMembers(ctx, tx, device, f.now)
			return err
		})
		return members
	}
	offer := func(ids ...int64) link.Offer {
		t.Helper()
		members := snapshot()
		if len(members) != 1 || len(members[0].Offers) != 1 || !slices.Equal(members[0].Offers[0].IDs, ids) {
			t.Fatalf("snapshot %+v, want offer for %v", members, ids)
		}
		return members[0].Offers[0]
	}
	first := offer(request.ID, message.ID)
	if first.Nonce == "" || first.Launch != "launch" || first.Epoch != 7 || first.PolicyEpoch != 7 ||
		first.LeafGeneration != "leaf" || first.Membership.Generation <= 0 || first.RouteEpoch <= 0 {
		t.Fatalf("incomplete offer %+v", first)
	}
	f.now = f.now.Add(10 * time.Second)
	if again := offer(request.ID, message.ID); again.OfferID != first.OfferID || again.LeaseMS != 50_000 {
		t.Fatalf("snapshot renewed an unexpired offer %+v", again)
	}
	update(func(tx store.Tx) error {
		if err := markLinkOfferDirty(ctx, tx, device, member, ""); err != nil {
			return err
		}
		members, err := snapshotLinkMembers(ctx, tx, device, f.now)
		if err == nil && (len(members) != 1 || len(members[0].Offers) != 0) {
			t.Fatalf("snapshot included an offer waiting on a dirty page: %+v", members)
		}
		return err
	})
	if replay := offer(request.ID, message.ID); replay.OfferID != first.OfferID || replay.LeaseMS != 50_000 {
		t.Fatalf("dirty page did not replay the unchanged offer's remaining lease: %+v", replay)
	}
	wantOK(t, f.owner(Cancel{Channel: "dev", ID: request.ID}, "cancel"))
	replacement := offer(message.ID)
	f.now = f.now.Add(linkOfferLease)
	renewal := offer(message.ID)
	if first.OfferID == replacement.OfferID || replacement.OfferID == renewal.OfferID || renewal.LeaseMS != 60_000 {
		t.Fatalf("replacement %+v, renewal %+v", replacement, renewal)
	}
	execArgs(f, `UPDATE offer SET written_at_ms = ? WHERE id = ?`, ms(f.now), first.OfferID)
	members := snapshot()
	if len(members) != 1 || len(members[0].Offers) != 0 {
		t.Fatalf("a written ID remained eligible: %+v", members)
	}
	var kinds string
	if err := f.db.QueryRow(`SELECT group_concat(kind || COALESCE('/' || json_extract(body, '$.reason'), ''), ',' ORDER BY seq)
		FROM device_event WHERE device_id = ?`, device).Scan(&kinds); err != nil {
		t.Fatal(err)
	}
	if want := "membership,offer,offer,invalidate/cancel,offer,invalidate/offer_set,offer,invalidate/offer_set"; kinds != want {
		t.Fatalf("events %s, want %s", kinds, want)
	}

	execArgs(f, `INSERT INTO link_event_cut
		(device_id, vendor, key, boot, leaf_generation, binding_id, membership_generation, route_epoch, leaf_seq, absent, event_seq)
		VALUES (?, ?, ?, 'boot', ?, ?, ?, ?, 0, 1, 1)`,
		device, member.Vendor, member.Key, first.LeafGeneration, first.BindingID, first.Membership.Generation, first.RouteEpoch)
	execArgs(f, `UPDATE binding SET route_epoch = route_epoch + 1 WHERE id = ?`, first.BindingID)
	if members := snapshot(); len(members) != 0 {
		t.Fatalf("a route change restored the absent leaf: %+v", members)
	}
	execArgs(f, `UPDATE device_key SET leaf_generation = 'next-leaf', leaf_epoch = 2
		WHERE device_id = ? AND vendor = ? AND key = ?`, device, member.Vendor, member.Key)
	if members := snapshot(); len(members) != 1 || members[0].Scope.LeafGeneration != "next-leaf" || len(members[0].Offers) != 0 {
		t.Fatalf("a new leaf generation did not restore only membership: %+v", members)
	}
}

func TestLinkOffersRefreshOneDevicePageAtATime(t *testing.T) {
	f := newFixture(t)
	device := decodeAs[LinkEnrollResult](t, f.owner(LinkEnroll{Name: "spoke", Secret: secretHex(1)}, "enroll")).Device
	other := decodeAs[LinkEnrollResult](t, f.owner(LinkEnroll{Name: "other", Secret: secretHex(2)}, "other")).Device
	execArgs(f, `WITH RECURSIVE pages(n) AS (VALUES (0) UNION ALL SELECT n + 1 FROM pages WHERE n < ?)
		INSERT INTO device_offer_dirty (device_id, vendor, key) SELECT ?, 'claude', printf('%03d', n) FROM pages`,
		linkOfferPageSize, device)
	execArgs(f, `INSERT INTO device_offer_dirty (device_id, vendor, key) VALUES (?, 'claude', 'remote')`, other)
	ctx := t.Context()
	tx, err := f.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if err := refreshLinkEvents(ctx, tx, device, f.now); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{device, other} {
		if n := f.count(`SELECT COUNT(*) FROM device_offer_dirty WHERE device_id = ?`, id); n != 1 {
			t.Fatalf("device %q: %d dirty rows left, want one", id, n)
		}
	}
}
