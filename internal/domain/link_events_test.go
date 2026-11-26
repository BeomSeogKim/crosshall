package domain

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/link"
)

func testLinkEventPeer(t *testing.T, f *fixture, name string) LinkEventPeer {
	t.Helper()
	device := decodeAs[LinkEnrollResult](t, f.owner(LinkEnroll{Name: name, Secret: secretHex(8)}, name)).Device
	return LinkEventPeer{Device: device, CredentialGen: 1, Admission: 1, Stream: link.NewNonce()}
}

func appendTestLinkEvent(t *testing.T, f *fixture, peer LinkEventPeer, kind string, payload any) uint64 {
	t.Helper()
	tx, err := f.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	seq, err := appendLinkEvent(t.Context(), tx, peer.Device, kind, payload, f.now)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return seq
}

func emptyLinkEventBaseline(t *testing.T, f *fixture, peer LinkEventPeer, boot string) {
	t.Helper()
	page := decodeAs[LinkEventsResult](t, f.owner(LinkEvents{LinkEventPeer: peer,
		Subscribe: &link.Subscribe{Boot: boot}}, ""))
	if page.Snapshot == nil || !page.Snapshot.Last || page.Snapshot.Seq != 0 || len(page.Snapshot.Members) != 0 {
		t.Fatalf("expected a complete empty snapshot: %+v", page)
	}
	wantOK(t, f.owner(LinkAck{LinkEventPeer: peer, Ack: link.Ack{Boot: boot}}, ""))
}

func TestHomeLinkCursorIsPrivateAndSurvivesCompleteTrim(t *testing.T) {
	f := newFixture(t)
	one, two := testLinkEventPeer(t, f, "one"), testLinkEventPeer(t, f, "two")
	const boot = "test-boot"
	emptyLinkEventBaseline(t, f, one, boot)
	emptyLinkEventBaseline(t, f, two, boot)

	seq := appendTestLinkEvent(t, f, one, link.EventOffer, link.Offer{
		Member: link.Member{Vendor: "claude", Key: "shared"}, OfferID: 2, IDs: []int64{8}, LeaseMS: 60000,
	})
	f.now = f.now.Add(15 * time.Second)
	page := decodeAs[LinkEventsResult](t, f.owner(LinkEvents{LinkEventPeer: one}, ""))
	if page.Through != seq || len(page.Events) != 1 || page.Events[0].Seq != seq {
		t.Fatalf("first device page: %+v", page)
	}
	var offer link.Offer
	if err := json.Unmarshal(page.Events[0].Body, &offer); err != nil || offer.LeaseMS != 45000 {
		t.Fatalf("remaining lease: %+v, %v", offer, err)
	}
	if page := decodeAs[LinkEventsResult](t, f.owner(LinkEvents{LinkEventPeer: two}, "")); page.Through != 0 || len(page.Events) != 0 {
		t.Fatalf("other device saw events: %+v", page)
	}
	wantCode(t, f.owner(LinkAck{LinkEventPeer: one, Ack: link.Ack{Seq: seq + 1, Boot: boot}}, ""),
		core.ExitRefused, "link_event_resync")
	wantOK(t, f.owner(LinkAck{LinkEventPeer: one, Ack: link.Ack{Seq: seq, Boot: boot}}, ""))
	if n := f.count(`SELECT COUNT(*) FROM device_event WHERE device_id = ?`, one.Device); n != 0 {
		t.Fatalf("acknowledged event remains: %d", n)
	}
	one.Stream = link.NewNonce()
	page = decodeAs[LinkEventsResult](t, f.owner(LinkEvents{LinkEventPeer: one,
		Subscribe: &link.Subscribe{FromSeq: seq, Boot: "after-reboot"}}, ""))
	if page.Snapshot == nil || page.Snapshot.Seq != seq || !page.Snapshot.Last {
		t.Fatalf("boot resync lost the durable cursor: %+v", page)
	}
}

func TestHomeLinkCutsCommitTogetherAndAbsentCutDoesNotInstallFence(t *testing.T) {
	f := newFixture(t)
	peer := testLinkEventPeer(t, f, "device")
	const boot = "test-boot"
	emptyLinkEventBaseline(t, f, peer, boot)
	scope := link.MemberScope{LeafGeneration: "g1", Membership: link.Membership{BindingID: 8, Generation: 2}, RouteEpoch: 3}
	gone := link.Member{Vendor: "claude", Key: "gone"}
	stays := link.Member{Vendor: "claude", Key: "stays"}
	first := appendTestLinkEvent(t, f, peer, link.EventMembership, link.MembershipChange{Member: gone, Scope: scope})
	last := appendTestLinkEvent(t, f, peer, link.EventMembership, link.MembershipChange{Member: stays, Scope: scope})
	wantOK(t, f.owner(LinkAck{LinkEventPeer: peer, Ack: link.Ack{Seq: last, Boot: boot, More: true,
		Cuts: []link.LeafCut{{Member: gone, Scope: scope, Seq: first, Absent: true}}}}, ""))
	if n := f.count(`SELECT COUNT(*) FROM link_event_cut`); n != 0 {
		t.Fatalf("partial acknowledgement installed %d cuts", n)
	}
	wantOK(t, f.owner(LinkAck{LinkEventPeer: peer, Ack: link.Ack{Seq: last, Boot: boot,
		Cuts: []link.LeafCut{{Member: stays, Scope: scope, Seq: last, LeafSeq: 7}}}}, ""))
	if n := f.count(`SELECT COUNT(*) FROM link_event_cut WHERE key = 'gone' AND absent = 1 AND leaf_seq = 0`); n != 1 {
		t.Fatalf("absent scope: %d rows", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM link_event_cut WHERE key = 'stays' AND absent = 0 AND leaf_seq = 7`); n != 1 {
		t.Fatalf("present scope: %d rows", n)
	}
}

func TestHomeLinkCannotRestoreSupersededSubscription(t *testing.T) {
	f := newFixture(t)
	old := testLinkEventPeer(t, f, "device")
	emptyLinkEventBaseline(t, f, old, "old-boot")
	newer := old
	newer.Stream = link.NewNonce()
	emptyLinkEventBaseline(t, f, newer, "new-boot")
	wantCode(t, f.owner(LinkEvents{LinkEventPeer: old, Resubscribe: true, Subscribe: &link.Subscribe{Boot: "old-boot", Snapshot: true}}, ""),
		core.ExitRefused, "link_event_resync")
	wantOK(t, f.owner(LinkEvents{LinkEventPeer: newer}, ""))
	if _, err := f.db.Exec(`UPDATE device SET admission_gen = admission_gen + 1 WHERE device_id = ?`, newer.Device); err != nil {
		t.Fatal(err)
	}
	wantCode(t, f.owner(LinkEvents{LinkEventPeer: newer}, ""), core.ExitRefused, CodeLinkStale)
}
