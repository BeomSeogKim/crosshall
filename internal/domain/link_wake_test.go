package domain

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/link"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

func TestLinkWakeOfflineUsesTheCurrentProvenStream(t *testing.T) {
	h, route := linkWakeFixture(t)
	check := func(want string) {
		t.Helper()
		got := inTx(t, h.fixture, func(ctx context.Context, tx store.Tx) (Wake, error) {
			return memberWake(ctx, tx, route.Scope.Membership.BindingID, h.now)
		})
		if got.Wake != WakeNone || got.Why != want {
			t.Fatalf("wake = %+v, want %s", got, want)
		}
	}
	check(WhyNoRoute)
	old := h.peer
	acked := uint64(h.count(`SELECT acked FROM link_event_cursor WHERE device_id = ?`, h.peer.Device))
	h.peer.Stream = link.NewNonce()
	wantOK(t, h.owner(LinkEvents{LinkEventPeer: h.peer, AllowWake: true,
		Subscribe: &link.Subscribe{Boot: route.Boot, FromSeq: acked}}, ""))
	wantOK(t, h.owner(LinkEvents{LinkEventPeer: old, Close: true}, ""))
	check(WhyNoRoute)

	wantOK(t, h.owner(LinkEvents{LinkEventPeer: h.peer, Close: true}, ""))
	wantCode(t, h.owner(LinkEvents{LinkEventPeer: h.peer}, ""), core.ExitRefused, "link_event_resync")
	check(WhyDeviceOffline)
	r := h.ask(h.writer, Ask{To: []string{"reader"}, New: true, Body: "Review while offline."}, "")
	wantOK(t, r)
	id := decode[AskResult](t, r).ID
	wake := inTx(t, h.fixture, func(ctx context.Context, tx store.Tx) (Wake, error) {
		return h.svc.messageWake(ctx, tx, route.Scope.Membership.BindingID, h.principal, id, h.now)
	})
	if wake.Why != WhyDeviceOffline || wake.SeenAt != SeenAtCommand {
		t.Fatalf("offline request = %+v", wake)
	}
	h.sql(`UPDATE link_event_launch SET verified = 0 WHERE principal_id = ?`, h.principal)
	check(WhyNoRoute)
}

func TestLinkWakeOfflineKeepsAnUnresolvedAttemptHeld(t *testing.T) {
	h, route := linkWakeFixture(t)
	id := h.turnEndAsk(h.writer, "reader", "held-while-offline")
	wake, _ := dispatchTestLinkWake(t, h, route, id)
	wantOK(t, h.owner(LinkEvents{LinkEventPeer: h.peer, Close: true}, ""))
	got := inTx(t, h.fixture, func(ctx context.Context, tx store.Tx) (Wake, error) {
		return memberWake(ctx, tx, route.Scope.Membership.BindingID, h.now)
	})
	if got.Why != WhyHeld {
		t.Fatalf("offline member with an unresolved attempt = %+v", got)
	}
	if n := h.count(`SELECT COUNT(*) FROM link_wake_attempt WHERE id = ? AND released_at_ms IS NULL`, wake.AttemptID); n != 1 {
		t.Fatal("stream closure released the unresolved attempt")
	}
	h.sql(`UPDATE link_wake_attempt SET released_at_ms = ? WHERE id = ?`, ms(h.now), wake.AttemptID)
	got = inTx(t, h.fixture, func(ctx context.Context, tx store.Tx) (Wake, error) {
		return h.svc.messageWake(ctx, tx, route.Scope.Membership.BindingID, h.principal, id, h.now)
	})
	if got.Why == WhyDeviceOffline {
		t.Fatalf("an already covered message was reported eligible: %+v", got)
	}
}

func linkWakeFixture(t *testing.T) (*linkedHookFixture, link.WakeRoute) {
	t.Helper()
	usePublishSender(t)
	h := newLinkedHookFixture(t)
	if err := store.InstallLinkWakeTriggers(t.Context(), h.db); err != nil {
		t.Fatal(err)
	}
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

func TestLinkWakeRetriesARefusalBeforeTheClaim(t *testing.T) {
	h, route := linkWakeFixture(t)
	id := h.turnEndAsk(h.writer, "reader", "retry-before-claim")
	wake, _ := dispatchTestLinkWake(t, h, route, id)
	ready := LinkWakeReady{LinkEventPeer: h.peer, Route: route}
	if got := decodeAs[LinkWakeReadyResult](t, h.owner(ready, "")); got.Replay == nil || got.Replay.AttemptID != wake.AttemptID {
		t.Fatalf("first replay = %+v", got)
	}
	record := func(attempt int64, cut uint64) {
		t.Helper()
		got := decodeAs[link.WakeRecorded](t, h.owner(LinkWakeResult{LinkEventPeer: h.peer,
			Result: link.WakeResult{AttemptID: attempt, Route: route, Outcome: link.WakeNotSubmitted, SubmissionSeq: cut}}, ""))
		if got.Status != link.RecordedApplied {
			t.Fatalf("recorded = %+v", got)
		}
	}
	// The replay could not find its local route. No transport claim was made.
	record(wake.AttemptID, 0)
	retry, _ := dispatchTestLinkWake(t, h, route, id)
	if retry.AttemptID == wake.AttemptID {
		t.Fatal("a refusal should start a new attempt on the same route")
	}
	record(retry.AttemptID, 7)
	if got := decodeAs[LinkWakeReadyResult](t, h.owner(ready, "")); got.Seq != 0 || got.Replay != nil {
		t.Fatalf("a refusal with a transport cut was retried: %+v", got)
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
		h.count(`SELECT COUNT(*) FROM link_wake_attempt WHERE id = ? AND first_result_at_ms < ?`, wake.AttemptID, ms(h.now)) != 1 {
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

func TestLinkWakeReleasesOnTurnCapturedBeforeResult(t *testing.T) {
	h, route := linkWakeFixture(t)
	id := h.turnEndAsk(h.writer, "reader", "first")
	wake, _ := dispatchTestLinkWake(t, h, route, id)
	h.fact(2, 4, prompt(route.Launch, "wake-turn", ms(h.now)), link.RecordedApplied)
	if h.count(`SELECT COUNT(*) FROM link_wake_attempt WHERE id = ? AND released_at_ms IS NULL AND activity_seq = 4`, wake.AttemptID) != 1 {
		t.Fatal("an early fact did not preserve causal evidence while dispatch was unresolved")
	}
	result := LinkWakeResult{LinkEventPeer: h.peer, Result: link.WakeResult{AttemptID: wake.AttemptID,
		Route: route, Outcome: link.WakeSubmitted, SubmissionSeq: 3}}
	if got := decodeAs[link.WakeRecorded](t, h.owner(result, "")); got.Status != link.RecordedApplied {
		t.Fatalf("wake result = %+v", got)
	}
	if h.count(`SELECT COUNT(*) FROM link_wake_attempt WHERE id = ? AND released_at_ms IS NOT NULL`, wake.AttemptID) != 1 {
		t.Fatal("result did not reconcile the later turn")
	}
	h.fact(3, 5, stop(route.Launch, "wake-turn", ms(h.now)+1), link.RecordedApplied)
	next := h.turnEndAsk(h.writer, "reader", "second")
	dispatchTestLinkWake(t, h, route, next)
}

func TestLinkWakeReleasesOnPickupAboveSubmissionCut(t *testing.T) {
	h, route := linkWakeFixture(t)
	id := h.turnEndAsk(h.writer, "reader", "wake")
	wake, _ := dispatchTestLinkWake(t, h, route, id)
	result := LinkWakeResult{LinkEventPeer: h.peer, Result: link.WakeResult{AttemptID: wake.AttemptID,
		Route: route, Outcome: link.WakeSubmitted, SubmissionSeq: 3}}
	if got := decodeAs[link.WakeRecorded](t, h.owner(result, "")); got.Status != link.RecordedApplied {
		t.Fatalf("wake result = %+v", got)
	}
	note := decodeAs[NoteResult](t, h.exec(h.caller(h.writer), Request{OpKey: "note",
		Command: Note{To: []string{"reader"}, Body: "another item"}}))
	pickup := func(id int64, seq uint64) {
		t.Helper()
		token := h.token
		token.LeafSeq = seq
		call := &LinkCall{Device: h.peer.Device, CredentialGen: h.peer.CredentialGen, LeafGeneration: token.LeafGeneration,
			Vendor: h.member.Vendor, Key: h.member.Key, WakePickup: &token}
		wantOK(t, h.exec(core.Caller{Role: core.RoleMember, PrincipalID: &h.principal},
			Request{Command: Read{IDs: []int64{id}}, LinkPickup: call}))
	}
	pickup(note.ID, 3)
	if h.count(`SELECT COUNT(*) FROM link_wake_attempt WHERE id = ? AND released_at_ms IS NULL`, wake.AttemptID) != 1 {
		t.Fatal("a pickup at the submission cut released the attempt")
	}
	pickup(id, 4)
	if h.count(`SELECT COUNT(*) FROM link_wake_attempt WHERE id = ? AND released_at_ms IS NOT NULL`, wake.AttemptID) != 1 {
		t.Fatal("a later pickup did not release the attempt")
	}
	next := h.turnEndAsk(h.writer, "reader", "next")
	dispatchTestLinkWake(t, h, route, next)
}

func TestLinkWakeUncertaintyWindowClosesOldAuthority(t *testing.T) {
	h, route := linkWakeFixture(t)
	id := h.turnEndAsk(h.writer, "reader", "wake")
	wake, seq := dispatchTestLinkWake(t, h, route, id)
	result := LinkWakeResult{LinkEventPeer: h.peer, Result: link.WakeResult{AttemptID: wake.AttemptID,
		Route: route, Outcome: link.WakeUncertain, SubmissionSeq: 2}}
	if got := decodeAs[link.WakeRecorded](t, h.owner(result, "")); got.Status != link.RecordedApplied {
		t.Fatalf("wake result = %+v", got)
	}
	next := h.turnEndAsk(h.writer, "reader", "next")
	h.now = h.now.Add(linkWakeHeldWindow - time.Millisecond)
	if got := decodeAs[link.WakeRecorded](t, h.owner(result, "")); got.Status != link.RecordedApplied {
		t.Fatalf("duplicate uncertainty = %+v", got)
	}
	ready := LinkWakeReady{LinkEventPeer: h.peer, Route: route}
	if got := decodeAs[LinkWakeReadyResult](t, h.owner(ready, "")); got.Seq != 0 {
		t.Fatalf("uncertain lane freed before its deadline: %+v", got)
	}
	h.now = h.now.Add(time.Millisecond)
	successor, _ := dispatchTestLinkWake(t, h, route, next)
	page := decodeAs[LinkEventsResult](t, h.owner(LinkEvents{LinkEventPeer: h.peer, FromSeq: seq - 1}, ""))
	var closed link.WakeClosed
	if len(page.Events) < 2 || page.Events[0].Seq != seq || page.Events[0].Kind != link.EventWakeClosed ||
		json.Unmarshal(page.Events[0].Body, &closed) != nil || closed.AttemptID != wake.AttemptID ||
		page.Events[1].Seq != seq+1 || page.Events[1].Kind != link.EventWake {
		t.Fatalf("closed wake changed the stream sequence: %+v", page)
	}
	result.Result.Outcome = link.WakeSubmitted
	if got := decodeAs[link.WakeRecorded](t, h.owner(result, "")); got.Status != link.RecordedApplied ||
		h.count(`SELECT COUNT(*) FROM link_wake_attempt WHERE id = ? AND released_at_ms IS NULL`, successor.AttemptID) != 1 ||
		h.count(`SELECT COUNT(*) FROM link_wake_attempt WHERE id = ? AND released_at_ms IS NOT NULL`, wake.AttemptID) != 1 {
		t.Fatalf("late result re-held the expired attempt: %+v", got)
	}
}

func TestLinkWakeAcceptsExactHistoricalResultAfterCredentialRotation(t *testing.T) {
	h, route := linkWakeFixture(t)
	id := h.turnEndAsk(h.writer, "reader", "wake")
	wake, _ := dispatchTestLinkWake(t, h, route, id)
	rotated := decodeAs[LinkEnrollResult](t, h.owner(LinkEnroll{Name: "macbook", Secret: secretHex(4)}, "rotate"))
	if !rotated.Rotated || rotated.Device != h.peer.Device {
		t.Fatalf("rotation = %+v", rotated)
	}
	result := LinkWakeResult{LinkEventPeer: h.peer, Result: link.WakeResult{AttemptID: wake.AttemptID,
		Route: route, Outcome: link.WakeNotSubmitted}}
	wantCode(t, h.owner(result, ""), core.ExitRefused, CodeLinkStale)
	peer := h.peer
	peer.CredentialGen++
	peer.Stream = link.NewNonce()
	wantOK(t, h.owner(LinkEvents{LinkEventPeer: peer, AllowWake: true,
		Subscribe: &link.Subscribe{Boot: "after-rotation"}}, ""))
	result.LinkEventPeer = peer
	if got := decodeAs[link.WakeRecorded](t, h.owner(result, "")); got.Status != link.RecordedApplied ||
		h.count(`SELECT COUNT(*) FROM link_wake_attempt WHERE id = ? AND credential_gen = 1 AND state = 'not_submitted'`, wake.AttemptID) != 1 {
		t.Fatalf("rotated device could not settle exact original attempt: %+v", got)
	}
}

func TestLinkWakeReleasesOnSupersessionOrFirstResultDeadline(t *testing.T) {
	cases := []struct {
		name, result, update string
		refine               bool
	}{
		{name: "new boot", result: link.WakeSubmitted, update: `UPDATE link_event_cursor SET boot = 'new-boot' WHERE device_id = (SELECT device_id FROM link_wake_attempt WHERE id = ?)`},
		{name: "new leaf", result: link.WakeSubmitted,
			update: `UPDATE device_key SET leaf_generation = 'new-leaf' WHERE principal_id = (SELECT principal_id FROM link_wake_attempt WHERE id = ?)`},
		{name: "absent leaf", result: link.WakeSubmitted,
			update: `UPDATE link_event_cut SET absent = 1 WHERE binding_id = (SELECT binding_id FROM link_wake_attempt WHERE id = ?)`},
		{name: "new membership", result: link.WakeSubmitted,
			update: `UPDATE member_selection SET generation = generation + 1 WHERE principal_id = (SELECT principal_id FROM link_wake_attempt WHERE id = ?)`},
		{name: "new credential", result: link.WakeSubmitted,
			update: `UPDATE device SET credential_gen = credential_gen + 1 WHERE device_id = (SELECT device_id FROM link_wake_attempt WHERE id = ?)`},
		{name: "unclaimed at credential rotation",
			update: `UPDATE device SET credential_gen = credential_gen + 1 WHERE device_id = (SELECT device_id FROM link_wake_attempt WHERE id = ?)`},
		{name: "depart", result: link.WakeSubmitted,
			update: `UPDATE binding SET state = 'departed', departed_at_ms = 12345 WHERE id = (SELECT binding_id FROM link_wake_attempt WHERE id = ?)`},
		{name: "submitted deadline", result: link.WakeSubmitted},
		{name: "uncertain deadline", result: link.WakeUncertain},
		{name: "refined deadline", result: link.WakeUncertain, refine: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, route := linkWakeFixture(t)
			id := h.turnEndAsk(h.writer, "reader", "first")
			wake, seq := dispatchTestLinkWake(t, h, route, id)
			result := LinkWakeResult{LinkEventPeer: h.peer, Result: link.WakeResult{
				AttemptID: wake.AttemptID, Route: route, Outcome: tc.result, SubmissionSeq: 2}}
			if tc.result != "" {
				wantOK(t, h.owner(result, ""))
			}
			if tc.update == "" {
				assertLinkWakeDeadline(t, h, route, result, tc.refine)
				return
			}
			tx, err := h.db.BeginTx(t.Context(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := tx.ExecContext(t.Context(), tc.update, wake.AttemptID); err != nil {
				t.Fatal(err)
			}
			var released bool
			if err := tx.QueryRowContext(t.Context(), `SELECT released_at_ms IS NOT NULL FROM link_wake_attempt WHERE id = ?`,
				wake.AttemptID).Scan(&released); err != nil || !released {
				t.Fatalf("scope change did not release in its writer: released=%t err=%v", released, err)
			}
			if tc.result == "" {
				r, err := linkEventPage(t.Context(), tx, h.peer.Device, seq-1, seq, h.now)
				if err != nil {
					t.Fatal(err)
				}
				page := decodeAs[LinkEventsResult](t, r)
				if len(page.Events) != 1 || page.Events[0].Seq != seq || page.Events[0].Kind != link.EventWakeClosed {
					t.Fatalf("superseded dispatch remained executable: %+v", page)
				}
				raw, err := json.Marshal(route)
				if err != nil {
					t.Fatal(err)
				}
				var channel int64
				if err := tx.QueryRowContext(t.Context(), `SELECT channel_id FROM binding WHERE id = ?`,
					route.Scope.Membership.BindingID).Scan(&channel); err != nil {
					t.Fatal(err)
				}
				scope := linkOfferScope{principal: h.principal, binding: hookBinding{channel: channel}}
				ids, err := pendingLinkWakeIDs(t.Context(), tx, scope, string(raw))
				if err != nil || slices.Contains(ids, id) {
					t.Fatalf("superseded dispatch admitted the same item: %v, %v", ids, err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func assertLinkWakeDeadline(t *testing.T, h *linkedHookFixture, route link.WakeRoute, result LinkWakeResult, refine bool) {
	t.Helper()
	until := h.now.Add(linkWakeHeldWindow)
	next := h.turnEndAsk(h.writer, "reader", "next")
	h.now = until.Add(-time.Millisecond)
	if refine {
		result.Result.Outcome = link.WakeSubmitted
	}
	wantOK(t, h.owner(result, ""))
	if got := decodeAs[LinkWakeReadyResult](t, h.owner(LinkWakeReady{LinkEventPeer: h.peer, Route: route}, "")); got.Seq != 0 {
		t.Fatalf("wake escaped before its deadline: %+v", got)
	}
	h.now = until
	dispatchTestLinkWake(t, h, route, next)
}

func TestLinkWakeNewBootReleasesOnlyAfterSnapshotAck(t *testing.T) {
	h, route := linkWakeFixture(t)
	id := h.turnEndAsk(h.writer, "reader", "wake")
	wake, _ := dispatchTestLinkWake(t, h, route, id)
	wantOK(t, h.owner(LinkWakeResult{LinkEventPeer: h.peer, Result: link.WakeResult{
		AttemptID: wake.AttemptID, Route: route, Outcome: link.WakeSubmitted, SubmissionSeq: 3}}, ""))
	h.peer.Stream = link.NewNonce()
	const boot = "restarted"
	page := decodeAs[LinkEventsResult](t, h.owner(LinkEvents{LinkEventPeer: h.peer, AllowWake: true,
		Subscribe: &link.Subscribe{Boot: boot}}, ""))
	if page.Snapshot == nil || !page.Snapshot.Last || len(page.Snapshot.Members) != 1 {
		t.Fatalf("new boot snapshot = %+v", page)
	}
	if h.count(`SELECT COUNT(*) FROM link_wake_attempt WHERE id = ? AND released_at_ms IS NULL`, wake.AttemptID) != 1 {
		t.Fatal("an unacknowledged boot released the existing scope")
	}
	wantOK(t, h.owner(LinkAck{LinkEventPeer: h.peer, Ack: link.Ack{Boot: boot, Seq: page.Snapshot.Seq,
		Cuts: []link.LeafCut{{Member: h.member, Scope: page.Snapshot.Members[0].Scope, Seq: page.Snapshot.Seq}}}}, ""))
	if h.count(`SELECT COUNT(*) FROM link_wake_attempt WHERE id = ? AND released_at_ms IS NOT NULL`, wake.AttemptID) != 1 {
		t.Fatal("the new acknowledged boot left the earlier scope held")
	}
}
