package domain

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/link"
)

func linkWakeFixture(t *testing.T) (*linkedHookFixture, link.WakeRoute) {
	t.Helper()
	usePublishSender(t)
	h := newLinkedHookFixture(t)
	const launch = "pid-42-wake"
	h.fact(1, 2, linkedStart(launch, ms(h.now)), link.RecordedApplied)
	h.sql(`INSERT INTO capability_evidence (vendor, vendor_version, platform, mode, manifest_rev, cap, level, result_ref, checked_at_ms)
		SELECT 'claude', json_extract(profile, '$.vendor_version'), json_extract(profile, '$.platform'),
			json_extract(profile, '$.mode'), json_extract(profile, '$.manifest_rev'), 'publish', 'tested', 'wake-test', 1
		FROM adapter_launch WHERE principal_id = ?`, h.principal)
	h.sql(`UPDATE link_event_stream SET wake_capable = 1 WHERE device_id = ?`, h.peer.Device)
	cut := h.installCut(2, false)
	page := decodeAs[LinkEventsResult](t, h.owner(LinkEvents{LinkEventPeer: h.peer}, ""))
	if len(page.Events) != 1 || page.Events[0].Kind != link.EventInvalidate {
		t.Fatalf("launch cut page = %+v", page)
	}
	cut.Seq = page.Through
	wantOK(t, h.owner(LinkAck{LinkEventPeer: h.peer,
		Ack: link.Ack{Boot: h.token.Boot, Seq: cut.Seq, Cuts: []link.LeafCut{cut}}}, ""))
	return h, link.WakeRoute{Member: h.member, Scope: cut.Scope,
		Boot: h.token.Boot, Launch: launch, PolicyEpoch: 1, Lane: link.NewNonce()}
}

func dispatchTestLinkWake(t *testing.T, h *linkedHookFixture, route link.WakeRoute, id int64) (link.Wake, uint64) {
	t.Helper()
	ready := decodeAs[LinkWakeReadyResult](t, h.owner(LinkWakeReady{LinkEventPeer: h.peer, Route: route}, ""))
	if ready.Seq == 0 || ready.Replay != nil {
		t.Fatalf("first authorization %+v", ready)
	}
	if n := h.count(`SELECT COUNT(*) FROM link_wake_attempt WHERE event_seq = ? AND state = 'dispatching'`, ready.Seq); n != 1 {
		t.Fatal("the wake event has no already committed dispatch fence")
	}
	page := decodeAs[LinkEventsResult](t, h.owner(LinkEvents{LinkEventPeer: h.peer, FromSeq: ready.Seq - 1}, ""))
	for _, event := range page.Events {
		if event.Seq != ready.Seq {
			continue
		}
		var wake link.Wake
		if event.Kind != link.EventWake || json.Unmarshal(event.Body, &wake) != nil ||
			wake.AttemptID <= 0 || wake.Route != route || !slices.Equal(wake.IDs, []int64{id}) {
			t.Fatalf("wake event = %+v, decoded %+v", event, wake)
		}
		return wake, event.Seq
	}
	t.Fatalf("missing event %d in %+v", ready.Seq, page)
	return link.Wake{}, 0
}

func TestLinkWakeDispatchSurvivesTrimAndReconnect(t *testing.T) {
	h, route := linkWakeFixture(t)
	id := h.turnEndAsk(h.writer, "reader", "wake")
	wake, seq := dispatchTestLinkWake(t, h, route, id)
	wantOK(t, h.owner(LinkAck{LinkEventPeer: h.peer, Ack: link.Ack{Seq: seq, Boot: route.Boot}}, ""))
	if h.count(`SELECT COUNT(*) FROM device_event WHERE device_id = ? AND seq = ?`, h.peer.Device, seq) != 0 {
		t.Fatal("acknowledged wake event was not trimmed")
	}
	old := h.peer
	h.peer.Stream = link.NewNonce()
	page := decodeAs[LinkEventsResult](t, h.owner(LinkEvents{LinkEventPeer: h.peer, AllowWake: true,
		Subscribe: &link.Subscribe{Boot: route.Boot, FromSeq: seq}}, ""))
	if page.Snapshot != nil {
		t.Fatalf("same boot reconnect did not resume: %+v", page)
	}
	wantCode(t, h.owner(LinkWakeReady{LinkEventPeer: old, Route: route}, ""), core.ExitRefused, "link_event_resync")
	h.svc = &Service{Clock: func() time.Time { return h.now }}
	h.now = h.now.Add(8 * 24 * time.Hour)
	replayed := decodeAs[LinkWakeReadyResult](t, h.owner(LinkWakeReady{LinkEventPeer: h.peer, Route: route}, ""))
	if replayed.Seq != seq || replayed.Replay == nil || replayed.Replay.AttemptID != wake.AttemptID ||
		!slices.Equal(replayed.Replay.IDs, wake.IDs) {
		t.Fatalf("replay after trim = %+v", replayed)
	}
	if n := h.count(`SELECT COUNT(*) FROM link_wake_attempt`); n != 1 {
		t.Fatalf("replay journal has %d attempts", n)
	}
}

func TestLinkWakeHeldLaneAndResultScope(t *testing.T) {
	h, route := linkWakeFixture(t)
	id := h.turnEndAsk(h.writer, "reader", "original-wake")
	wake, _ := dispatchTestLinkWake(t, h, route, id)
	result := link.WakeResult{AttemptID: wake.AttemptID, Route: route, Outcome: link.WakeUncertain, SubmissionSeq: 2}
	record := func(result link.WakeResult) link.WakeRecorded {
		t.Helper()
		return decodeAs[link.WakeRecorded](t, h.owner(LinkWakeResult{LinkEventPeer: h.peer, Result: result}, ""))
	}
	if got := record(result); got.Status != link.RecordedApplied {
		t.Fatalf("uncertain result = %+v", got)
	}
	h.now = h.now.Add(time.Minute)
	if got := record(result); got.Status != link.RecordedApplied ||
		h.count(`SELECT COUNT(*) FROM link_wake_attempt WHERE id = ? AND resolved_at_ms < ?`, wake.AttemptID, ms(h.now)) != 1 {
		t.Fatalf("uncertain result replay = %+v", got)
	}
	foreign := h.peer
	foreign.Device, foreign.Stream = "dev-b", link.NewNonce()
	h.sql(`INSERT INTO link_event_cursor (device_id) VALUES (?)`, foreign.Device)
	h.sql(`INSERT INTO link_event_stream (device_id, stream, admission_gen, boot, created_at_ms, wake_capable)
		VALUES (?, ?, 1, ?, ?, 1)`, foreign.Device, foreign.Stream, route.Boot, ms(h.now))
	negative := result
	negative.Outcome = link.WakeNotSubmitted
	if got := decodeAs[link.WakeRecorded](t, h.owner(LinkWakeResult{LinkEventPeer: foreign, Result: negative}, "")); got.Code != "wake_scope" {
		t.Fatalf("foreign device settled the attempt: %+v", got)
	}
	other := result
	other.Outcome, other.Route.Lane = link.WakeNotSubmitted, link.NewNonce()
	if got := record(other); got.Status != link.RecordedStale || got.Code != "wake_scope" {
		t.Fatalf("foreign lane settled the attempt: %+v", got)
	}
	h.sql(`UPDATE binding SET route_epoch = route_epoch + 1 WHERE id = ?`, route.Scope.Membership.BindingID)
	h.sql(`UPDATE link_event_launch SET route_epoch = route_epoch + 1 WHERE principal_id = ?`, h.principal)
	newRoute := route
	newRoute.Scope = h.installCut(3, false).Scope
	if held := decodeAs[LinkWakeReadyResult](t, h.owner(LinkWakeReady{LinkEventPeer: h.peer, Route: newRoute}, "")); held.Seq != 0 {
		t.Fatalf("rebound route bypassed a held lane: %+v", held)
	}
	result.Outcome = link.WakeNotSubmitted
	if got := record(result); got.Status != link.RecordedApplied {
		t.Fatalf("late original negative = %+v", got)
	}
	wantOK(t, h.owner(Cancel{Channel: "checkout", ID: id}, "cancel-old"))
	next := h.turnEndAsk(h.writer, "reader", "next-wake")
	newWake, _ := dispatchTestLinkWake(t, h, newRoute, next)
	result.Outcome = link.WakeSubmitted
	if got := record(result); got.Status != link.RecordedStale ||
		h.count(`SELECT COUNT(*) FROM link_wake_attempt WHERE id = ? AND state = 'dispatching'`, newWake.AttemptID) != 1 {
		t.Fatalf("late contradictory result touched successor: %+v", got)
	}
}

func TestLegacyEventSubscriberSkipsWakeThroughSnapshot(t *testing.T) {
	h, route := linkWakeFixture(t)
	prior := appendTestLinkEvent(t, h.fixture, h.peer, link.EventOffer, link.Offer{})
	wantOK(t, h.owner(LinkAck{LinkEventPeer: h.peer, Ack: link.Ack{Seq: prior, Boot: route.Boot}}, ""))
	id := h.turnEndAsk(h.writer, "reader", "wake")
	_, seq := dispatchTestLinkWake(t, h, route, id)
	h.peer.Stream = link.NewNonce()
	page := decodeAs[LinkEventsResult](t, h.owner(LinkEvents{LinkEventPeer: h.peer,
		Subscribe: &link.Subscribe{Boot: route.Boot, FromSeq: prior}}, ""))
	if page.Snapshot == nil || page.Snapshot.Seq < seq || len(page.Events) != 0 {
		t.Fatalf("events-only peer must cross the wake via a snapshot: %+v", page)
	}
	wantCode(t, h.owner(LinkWakeReady{LinkEventPeer: h.peer, Route: route}, ""), core.ExitRefused, "link_event_resync")
	if n := h.count(`SELECT COUNT(*) FROM link_wake_attempt WHERE state = 'dispatching'`); n != 1 {
		t.Fatal("a legacy subscription released the held wake")
	}
}

func TestLinkWakeWaitsForCurrentRouteCut(t *testing.T) {
	h, route := linkWakeFixture(t)
	id := h.turnEndAsk(h.writer, "reader", "wake")
	h.sql(`INSERT INTO device_offer_dirty (device_id, vendor, key, invalidate_reason)
		VALUES (?, ?, ?, 'route')`, h.peer.Device, h.member.Vendor, h.member.Key)
	ready := LinkWakeReady{LinkEventPeer: h.peer, Route: route}
	if got := decodeAs[LinkWakeReadyResult](t, h.owner(ready, "")); got.Seq != 0 {
		t.Fatalf("dirty route authorized a wake: %+v", got)
	}
	page := decodeAs[LinkEventsResult](t, h.owner(LinkEvents{LinkEventPeer: h.peer, FromSeq: 1}, ""))
	if got := decodeAs[LinkWakeReadyResult](t, h.owner(ready, "")); got.Seq != 0 {
		t.Fatalf("unacknowledged route authorized a wake: %+v", got)
	}
	for _, event := range page.Events {
		if event.Kind != link.EventInvalidate {
			continue
		}
		wantOK(t, h.owner(LinkAck{LinkEventPeer: h.peer, Ack: link.Ack{Boot: route.Boot, Seq: page.Through,
			Cuts: []link.LeafCut{{Member: h.member, Scope: route.Scope, Seq: event.Seq, LeafSeq: 3}}}}, ""))
		dispatchTestLinkWake(t, h, route, id)
		return
	}
	t.Fatalf("no route invalidation in %+v", page)
}
