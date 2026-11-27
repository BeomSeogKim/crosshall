package domain

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/link"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

type linkedHookFixture struct {
	*fixture
	writer    session
	peer      LinkEventPeer
	member    link.Member
	token     link.HookToken
	principal int64
}

func newLinkedHookFixture(t *testing.T) *linkedHookFixture {
	t.Helper()
	f, writer := adapterChannel(t)
	reader := f.newSession("claude")
	wantOK(t, f.join(reader, Join{Channel: "checkout", Alias: "reader"}, ""))
	principal := f.principalOf(reader)
	membership, err := SelectedMembership(t.Context(), f.db, principal)
	if err != nil {
		t.Fatal(err)
	}
	h := &linkedHookFixture{
		fixture: f,
		writer:  writer,
		peer:    LinkEventPeer{Device: "dev-a", CredentialGen: 1, Admission: 1, Stream: link.NewNonce()},
		member:  link.Member{Vendor: reader.vendor, Key: reader.key},
		token: link.HookToken{
			Boot: "boot-a", WallMS: ms(f.now), LeafGeneration: "gen-a",
			Membership: link.Membership{BindingID: membership.BindingID, Generation: membership.Generation},
		},
		principal: principal,
	}
	h.sql(`INSERT INTO device (device_id, name, secret, created_at_ms, secret_at_ms)
		VALUES ('dev-a', 'macbook', zeroblob(32), 0, 0), ('dev-b', 'mini', zeroblob(32), 0, 0)`)
	h.sql(`UPDATE principal SET device_id = ? WHERE id = ?`, h.peer.Device, principal)
	h.sql(`INSERT INTO device_key (device_id, vendor, key, principal_id, leaf_generation, leaf_epoch, credential_gen, state, created_at_ms)
		VALUES (?, ?, ?, ?, ?, 1, 1, 'live', 0)`, h.peer.Device, h.member.Vendor, h.member.Key, principal, h.token.LeafGeneration)
	h.sql(`DELETE FROM leaf WHERE digest = ?`, reader.digest())
	h.sql(`INSERT INTO link_event_cursor (device_id, boot, credential_gen) VALUES (?, ?, 1)`, h.peer.Device, h.token.Boot)
	h.sql(`INSERT INTO link_event_stream (device_id, stream, admission_gen, boot, created_at_ms) VALUES (?, ?, ?, ?, ?)`,
		h.peer.Device, h.peer.Stream, h.peer.Admission, h.token.Boot, ms(f.now))
	h.installCut(1, false)
	f.svc.LaunchProcess = func(string) (string, bool) {
		t.Fatal("home attempted to inspect a linked launch")
		return "", false
	}
	return h
}

func (h *linkedHookFixture) sql(cmd string, args ...any) {
	h.t.Helper()
	if _, err := h.db.Exec(cmd, args...); err != nil {
		h.t.Fatal(err)
	}
}

func (h *linkedHookFixture) installCut(seq uint64, absent bool) link.LeafCut {
	h.t.Helper()
	epoch := h.count(`SELECT route_epoch FROM binding WHERE id = ?`, h.token.Membership.BindingID)
	cut := link.LeafCut{
		Member:  h.member,
		LeafSeq: seq,
		Absent:  absent,
		Scope: link.MemberScope{
			LeafGeneration: h.token.LeafGeneration,
			Membership:     h.token.Membership,
			RouteEpoch:     int64(epoch),
		},
	}
	h.sql(`INSERT OR REPLACE INTO link_event_cut
		(device_id, vendor, key, boot, leaf_generation, binding_id, membership_generation, route_epoch, leaf_seq, absent, event_seq)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0)`, h.peer.Device, h.member.Vendor, h.member.Key, h.token.Boot,
		h.token.LeafGeneration, h.token.Membership.BindingID, h.token.Membership.Generation, epoch, seq, absent)
	return cut
}

func (h *linkedHookFixture) fact(id, seq uint64, cmd Command, status string) {
	h.t.Helper()
	if got := decodeAs[link.Recorded](h.t, h.owner(h.factCommand(id, seq, cmd), "")); got.ID != id || got.Status != status {
		h.t.Fatalf("fact %d at %d = %+v, want %s", id, seq, got, status)
	}
}

func (h *linkedHookFixture) factCommand(id, seq uint64, cmd Command) LinkFact {
	h.t.Helper()
	raw, err := json.Marshal(cmd)
	if err != nil {
		h.t.Fatal(err)
	}
	token := h.token
	token.LeafSeq = seq
	return LinkFact{LinkEventPeer: h.peer, Fact: link.Fact{ID: id, Member: h.member, Token: token, Kind: cmd.Verb(), Body: raw}}
}

func linkedStart(launch string, at int64) HookEnroll {
	cmd := start(launch, "startup", at)
	cmd.RepoRoot = ""
	return cmd
}

func TestLinkFactsUseCaptureOrderAndReplay(t *testing.T) {
	h := newLinkedHookFixture(t)
	launch := "pid-42-first"
	now := ms(h.now)
	h.fact(1, 2, linkedStart(launch, now), link.RecordedApplied)
	prompt := prompt(launch, "t1", now+86_400_000)
	h.token.WallMS = prompt.HookAtMS
	h.fact(2, 3, prompt, link.RecordedApplied)
	h.fact(2, 3, prompt, link.RecordedApplied)
	stop := stop(launch, "t1", now-86_400_000)
	h.token.WallMS = stop.HookAtMS
	h.fact(3, 4, stop, link.RecordedApplied)
	if got := h.count(`SELECT COUNT(*) FROM turn WHERE principal_id = ? AND finishing_hook_at_ms > opened_hook_at_ms`, h.principal); got != 1 {
		t.Fatalf("turns closed by capture order = %d", got)
	}
	if got := h.count(`SELECT COUNT(*) FROM observation WHERE principal_id = ? AND kind = 'prompt_submitted'`, h.principal); got != 1 {
		t.Fatalf("prompt observations after replay = %d", got)
	}
	h.fact(4, 3, prompt, link.RecordedQuarantined)
	h.fact(5, 7, linkedStart("pid-42-second", now+1), link.RecordedQuarantined)
	cut := h.installCut(2, false)
	if err := h.svc.resolveLinkFactCuts(t.Context(), h.db, h.peer, link.Ack{Boot: h.token.Boot, Cuts: []link.LeafCut{cut}}, true, h.now); err != nil {
		t.Fatal(err)
	}
	if got := h.count(`SELECT COUNT(*) FROM link_event_input WHERE id IN (4, 5) AND status = 'quarantined'`); got != 2 {
		t.Fatalf("records still quarantined after a rewound snapshot = %d", got)
	}
	cut = h.installCut(6, false)
	if err := h.svc.resolveLinkFactCuts(t.Context(), h.db, h.peer, link.Ack{Boot: h.token.Boot, Cuts: []link.LeafCut{cut}}, true, h.now); err != nil {
		t.Fatal(err)
	}
	if got := h.count(`SELECT COUNT(*) FROM link_event_input WHERE id = 4 AND status = 'stale' OR id = 5 AND status = 'applied'`); got != 2 {
		t.Fatalf("records resolved after the snapshot = %d", got)
	}
}

func TestLinkFactEndMatchesActiveRemoteLaunch(t *testing.T) {
	h := newLinkedHookFixture(t)
	now := ms(h.now)
	h.fact(1, 2, linkedStart("pid-42-first", now), link.RecordedApplied)
	h.fact(2, 3, linkedStart("pid-42-second", now+1), link.RecordedApplied)
	h.fact(3, 4, ended("pid-42-first", now+2), link.RecordedStale)
	cmd := h.factCommand(4, 5, ended("pid-42-second", now+3))
	stalePeer := cmd
	stalePeer.Device = "dev-b"
	h.sql(`INSERT INTO link_event_cursor (device_id, boot, credential_gen) VALUES (?, ?, 1)`, stalePeer.Device, h.token.Boot)
	h.sql(`INSERT INTO link_event_stream (device_id, stream, admission_gen, boot, created_at_ms) VALUES (?, ?, ?, ?, ?)`,
		stalePeer.Device, stalePeer.Stream, stalePeer.Admission, h.token.Boot, now)
	if got := decodeAs[link.Recorded](t, h.owner(stalePeer, "")); got.Status != link.RecordedStale {
		t.Fatalf("foreign device ended a member: %+v", got)
	}
	probes, err := LiveProcessLaunches(t.Context(), h.db, 0, 10)
	if err != nil || len(probes) != 0 {
		t.Fatalf("home process-sweep candidates = %+v, %v", probes, err)
	}
	if got := h.count(`SELECT COUNT(*) FROM binding WHERE id = ? AND state = 'live'`, h.token.Membership.BindingID); got != 1 {
		t.Fatal("a stale end changed the live binding")
	}
	if got := decodeAs[link.Recorded](t, h.owner(cmd, "")); got.Status != link.RecordedApplied {
		t.Fatalf("current end = %+v", got)
	}
	if got := h.count(`SELECT COUNT(*) FROM binding WHERE id = ? AND state = 'departed' AND departed_by_hook = 1`, h.token.Membership.BindingID); got != 1 {
		t.Fatal("the current end did not depart its binding")
	}
}

func TestLinkWrittenMatchesRemoteOfferScope(t *testing.T) {
	h := newLinkedHookFixture(t)
	launch := "pid-42-first"
	h.fact(1, 2, linkedStart(launch, ms(h.now)), link.RecordedApplied)
	h.sql(`INSERT INTO offer (id, principal_id, binding_id, route_epoch, generation, launch_ref, epoch,
		reason, evidence, at_ms, receipt_mode, nonce, device_credential_gen, device_membership_gen)
		SELECT 17, principal_id, binding_id, route_epoch, leaf_generation, launch_ref, policy_epoch,
		'prompt', 'fixture', 0, 'provisional', 'nonce', credential_gen, membership_generation FROM link_event_launch`)
	h.token.LeafSeq = 3
	cmd := LinkWritten{LinkEventPeer: h.peer, Written: link.Written{
		ID: 2, Member: h.member, OfferID: 17, Nonce: "wrong", WrittenAtMS: ms(h.now.Add(time.Hour)), Token: h.token,
	}}
	if got := decodeAs[link.Recorded](t, h.owner(cmd, "")); got.Status != link.RecordedStale {
		t.Fatalf("wrong nonce = %+v", got)
	}
	cmd.Written.ID = 3
	cmd.Written.Nonce = "nonce"
	if got := decodeAs[link.Recorded](t, h.owner(cmd, "")); got.Status != link.RecordedApplied {
		t.Fatalf("valid receipt = %+v", got)
	}
	if got := h.count(`SELECT COUNT(*) FROM offer WHERE id = 17 AND written_at_ms = ?`, ms(h.now)); got != 1 {
		t.Fatal("the linked receipt did not record the home write time")
	}
}

// A display receipt handled after a later fact of its leaf (#426, Sol
// #3696) still confirms its offer: it is not a lifecycle step, so it
// neither rewinds the leaf nor moves its head.
func TestALateLinkReceiptConfirmsItsOfferWithoutRewinding(t *testing.T) {
	h := newLinkedHookFixture(t)
	launch := "pid-42-first"
	h.fact(1, 2, linkedStart(launch, ms(h.now)), link.RecordedApplied)
	h.sql(`INSERT INTO offer (id, principal_id, binding_id, route_epoch, generation, launch_ref, epoch,
		reason, evidence, at_ms, receipt_mode, nonce, device_credential_gen, device_membership_gen)
		SELECT 17, principal_id, binding_id, route_epoch, leaf_generation, launch_ref, policy_epoch,
		'prompt', 'fixture', 0, 'provisional', 'nonce', credential_gen, membership_generation FROM link_event_launch`)
	h.fact(2, 9, prompt(launch, "t1", ms(h.now)), link.RecordedApplied)
	h.token.LeafSeq = 8
	late := LinkWritten{LinkEventPeer: h.peer, Written: link.Written{
		ID: 3, Member: h.member, OfferID: 17, Nonce: "nonce", WrittenAtMS: ms(h.now), Token: h.token,
	}}
	if got := decodeAs[link.Recorded](t, h.owner(late, "")); got.Status != link.RecordedApplied {
		t.Fatalf("late receipt = %+v", got)
	}
	if got := h.count(`SELECT COUNT(*) FROM offer WHERE id = 17 AND written_at_ms IS NOT NULL`); got != 1 {
		t.Fatal("the late receipt did not confirm its offer")
	}
	if got := h.count(`SELECT COUNT(*) FROM link_event_fact_head WHERE leaf_seq = 9 AND quarantined = 0`); got != 1 {
		t.Fatal("the late receipt moved or quarantined the leaf's head")
	}
	h.fact(4, 10, stop(launch, "t1", ms(h.now)), link.RecordedApplied)
}

func TestALinkReceiptCapturedBeforeSessionEndCanConfirmItsOffer(t *testing.T) {
	for _, tc := range []struct {
		name      string
		seq       uint64
		nonce     string
		absentCut bool
		change    string
		confirmed bool
	}{
		{name: "receipt before end", seq: 8, nonce: "nonce", confirmed: true},
		{name: "depart cut already acknowledged", seq: 8, nonce: "nonce", absentCut: true, confirmed: true},
		{name: "same sequence", seq: 9, nonce: "nonce"},
		{name: "receipt after end", seq: 10, nonce: "nonce"},
		{name: "wrong nonce", seq: 8, nonce: "wrong"},
		{name: "old membership", seq: 8, nonce: "nonce", change: `UPDATE member_selection SET generation = generation + 1 WHERE binding_id IN (SELECT id FROM binding WHERE state = 'departed')`},
		{name: "route changed", seq: 8, nonce: "nonce", change: `UPDATE binding SET route_epoch = route_epoch + 1 WHERE state = 'departed'`},
		{name: "policy changed", seq: 8, nonce: "nonce", change: `UPDATE adapter_policy SET epoch = epoch + 1 WHERE adapter = 'claude'`},
		{name: "terminal proof missing", seq: 8, nonce: "nonce", change: `DELETE FROM observation WHERE kind = 'session_ended'`},
		{name: "leaf quarantined", seq: 8, nonce: "nonce", change: `UPDATE link_event_fact_head SET quarantined = 1`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newLinkedHookFixture(t)
			const launch = "pid-42-first"
			h.fact(1, 2, linkedStart(launch, ms(h.now)), link.RecordedApplied)
			h.sql(`INSERT INTO offer (id, principal_id, binding_id, route_epoch, generation, launch_ref, epoch,
				reason, evidence, at_ms, receipt_mode, nonce, device_credential_gen, device_membership_gen)
				SELECT 17, principal_id, binding_id, route_epoch, leaf_generation, launch_ref, policy_epoch,
				'prompt', 'fixture', 0, 'provisional', 'nonce', credential_gen, membership_generation FROM link_event_launch`)
			h.fact(2, 7, prompt(launch, "t1", ms(h.now)), link.RecordedApplied)
			h.fact(3, 9, ended(launch, ms(h.now)), link.RecordedApplied)
			if tc.absentCut {
				h.installCut(9, true)
			}
			if tc.change != "" {
				h.sql(tc.change)
			}
			h.token.LeafSeq = tc.seq
			receipt := LinkWritten{LinkEventPeer: h.peer, Written: link.Written{
				ID: 4, Member: h.member, OfferID: 17, Nonce: tc.nonce, WrittenAtMS: ms(h.now), Token: h.token,
			}}
			r := decodeAs[link.Recorded](t, h.owner(receipt, ""))
			if (r.Status == link.RecordedApplied) != tc.confirmed {
				t.Fatalf("receipt at %d following end at 9: %+v", tc.seq, r)
			}
			if n := h.count(`SELECT COUNT(*) FROM offer WHERE id = 17 AND written_at_ms IS NOT NULL`); (n == 1) != tc.confirmed {
				t.Fatalf("confirmed %d offers", n)
			}
			if tc.confirmed {
				if r := decodeAs[link.Recorded](t, h.owner(receipt, "")); r.Status != link.RecordedApplied {
					t.Fatalf("receipt replay: %+v", r)
				}
			}
			if n := h.count(`SELECT COUNT(*) FROM link_event_fact_head WHERE leaf_seq = 9`); n != 1 {
				t.Fatal("receipt moved the terminal head")
			}
			if n := h.count(`SELECT COUNT(*) FROM binding WHERE id = ? AND state = 'departed' AND departed_by_hook = 1`, h.token.Membership.BindingID); n != 1 {
				t.Fatal("receipt changed the departed binding")
			}
		})
	}
}

func TestLinkFactRejectsLocalProcessFields(t *testing.T) {
	h := newLinkedHookFixture(t)
	cmd := linkedStart("pid-42-first", ms(h.now))
	cmd.Host = &wire.Host{PID: 42, Start: 1}
	invalid := h.factCommand(1, 2, cmd)
	for range 2 {
		got := decodeAs[link.Recorded](t, h.owner(invalid, ""))
		if got.ID != 1 || got.Status != link.RecordedStale || got.Code != "invalid_input" {
			t.Fatalf("forwarded local process data receipt = %+v", got)
		}
	}
	if got := h.count(`SELECT COUNT(*) FROM link_event_input WHERE id = 1 AND code = 'invalid_input'`); got != 1 {
		t.Fatalf("invalid input receipt rows = %d", got)
	}
	conflict := h.factCommand(1, 2, linkedStart("pid-42-first", ms(h.now)))
	got := decodeAs[link.Recorded](t, h.owner(conflict, ""))
	if got.Status != link.RecordedStale || got.Code != string(core.CodeKeyConflict) {
		t.Fatalf("different input reused the terminal receipt: %+v", got)
	}
	h.fact(2, 2, linkedStart("pid-42-first", ms(h.now)), link.RecordedApplied)
}

func TestLinkFactNewBootWaitsForSnapshot(t *testing.T) {
	h := newLinkedHookFixture(t)
	now := ms(h.now)
	h.fact(1, 8, linkedStart("pid-42-old", now), link.RecordedApplied)
	replay := h.factCommand(1, 8, linkedStart("pid-42-old", now))
	old := h.factCommand(3, 9, ended("pid-42-old", now+2))
	h.token.Boot = "boot-b"
	h.peer.Stream = link.NewNonce()
	h.sql(`DELETE FROM link_event_stream WHERE device_id = ?`, h.peer.Device)
	h.sql(`INSERT INTO link_event_stream (device_id, stream, admission_gen, boot, snapshot_seq, created_at_ms) VALUES (?, ?, ?, ?, 0, ?)`,
		h.peer.Device, h.peer.Stream, h.peer.Admission, h.token.Boot, now)
	h.fact(1, 3, linkedStart("pid-42-new", now+1), link.RecordedQuarantined)
	if got := h.count(`SELECT COUNT(*) FROM link_event_launch WHERE principal_id = ? AND boot = 'boot-a'`, h.principal); got != 1 {
		t.Fatal("an unacknowledged boot changed the active launch")
	}
	h.sql(`UPDATE link_event_cursor SET boot = ? WHERE device_id = ?`, h.token.Boot, h.peer.Device)
	cut := h.installCut(2, false)
	if err := h.svc.resolveLinkFactCuts(t.Context(), h.db, h.peer, link.Ack{Boot: h.token.Boot, Cuts: []link.LeafCut{cut}}, true, h.now); err != nil {
		t.Fatal(err)
	}
	if got := h.count(`SELECT COUNT(*) FROM link_event_launch WHERE principal_id = ? AND boot = 'boot-b' AND launch_ref = 'pid-42-new'`, h.principal); got != 1 {
		t.Fatal("the post-cut fact did not install the new launch")
	}
	if got := h.count(`SELECT COUNT(*) FROM link_event_input WHERE id = 1 AND status = 'applied'`); got != 2 {
		t.Fatalf("outbox IDs reused across two boots recorded %d rows", got)
	}
	wantCode(t, h.owner(old, ""), core.ExitRefused, "link_event_resync")
	replay.LinkEventPeer = h.peer
	if got := decodeAs[link.Recorded](t, h.owner(replay, "")); got.Status != link.RecordedApplied {
		t.Fatalf("already committed fact replayed from the new stream = %+v", got)
	}
}

func TestLinkFactsWaitForUnacknowledgedRouteCut(t *testing.T) {
	h := newLinkedHookFixture(t)
	now := ms(h.now)
	launch := "pid-42-first"
	h.fact(1, 2, linkedStart(launch, now), link.RecordedApplied)
	scope, found, err := currentLinkHookScope(t.Context(), h.db, h.peer, h.member)
	if err != nil || !found {
		t.Fatalf("linked hook scope = %+v, %v, %v", scope, found, err)
	}
	raw, err := json.Marshal(link.Invalidate{Member: h.member, Scope: scope.MemberScope, Reason: link.InvalidateRoute})
	if err != nil {
		t.Fatal(err)
	}
	h.sql(`INSERT INTO device_event (device_id, seq, kind, body, at_ms) VALUES (?, 1, ?, ?, ?)`,
		h.peer.Device, link.EventInvalidate, string(raw), now)
	h.fact(2, 3, prompt(launch, "before-cut", now), link.RecordedQuarantined)
	h.fact(3, 4, prompt(launch, "after-cut", now), link.RecordedQuarantined)
	cut := h.installCut(3, false)
	cut.Seq = 1
	h.sql(`UPDATE link_event_cut SET event_seq = ? WHERE device_id = ?`, cut.Seq, h.peer.Device)
	if err := h.svc.resolveLinkFactCuts(t.Context(), h.db, h.peer, link.Ack{Boot: h.token.Boot, Cuts: []link.LeafCut{cut}}, false, h.now); err != nil {
		t.Fatal(err)
	}
	if got := h.count(`SELECT COUNT(*) FROM turn WHERE principal_id = ? AND turn_ref = 'after-cut'`, h.principal); got != 1 {
		t.Fatal("the cut did not release the later capture")
	}
	if got := h.count(`SELECT COUNT(*) FROM turn WHERE principal_id = ? AND turn_ref = 'before-cut'`, h.principal); got != 0 {
		t.Fatal("a capture at the cut became a turn")
	}
}

func TestLinkFactDoesNotEnableTurnEndOrHomePromptProbe(t *testing.T) {
	h := newLinkedHookFixture(t)
	wantOK(t, h.owner(turnEndEvidence("tested"), "evidence"))
	h.svc.HasPromptLine = func(string, string) bool {
		t.Fatal("home probed the linked adapter installation")
		return false
	}
	launch, now := "pid-42-first", ms(h.now)
	h.fact(1, 2, linkedStart(launch, now), link.RecordedApplied)
	h.fact(2, 3, prompt(launch, "t1", now+1), link.RecordedApplied)
	h.turnEndAsk(h.writer, "reader", "held until the next prompt")
	waiting, err := TurnEndAskWaiting(t.Context(), h.db, h.principal)
	if err != nil || waiting {
		t.Fatalf("device turn-end waiting = %t (%v)", waiting, err)
	}
	covered, err := turnEndCovered(t.Context(), h.db, h.principal)
	if err != nil || covered {
		t.Fatalf("device turn-end coverage = %t (%v)", covered, err)
	}
	h.fact(3, 4, stop(launch, "t1", now+2), link.RecordedApplied)
	if got := h.count(`SELECT COUNT(*) FROM turn_end_notice WHERE principal_id = ?`, h.principal); got != 0 {
		t.Fatalf("linked turn-end claims = %d", got)
	}
}

// The owner sees why a member on a linked device that joined after its
// session started has no route yet; its first prompt opens the launch and
// clears the hint (#437).
func TestTheOwnerSeesALinkedMemberWhoseStartWasNotSeen(t *testing.T) {
	h := newLinkedHookFixture(t)
	if !h.startUnseen("reader") {
		t.Fatal("a linked member with no launch shows no unseen start")
	}
	const launch = "pid-42-joined"
	at := ms(h.now)
	h.token.WallMS = at
	h.fact(1, 2, prompt(launch, "t1", at), link.RecordedApplied)
	if h.startUnseen("reader") {
		t.Fatal("a linked member with a launch still shows an unseen start")
	}
	// A rejoin or a move gives a new scope the launch does not serve.
	binding := h.token.Membership.BindingID
	h.sql(`UPDATE binding SET route_epoch = route_epoch + 1 WHERE id = ?`, binding)
	if !h.startUnseen("reader") {
		t.Fatal("a launch of an earlier route epoch hid the unseen start")
	}
	h.sql(`UPDATE binding SET route_epoch = route_epoch - 1 WHERE id = ?`, binding)
	h.sql(`UPDATE member_selection SET generation = generation + 1 WHERE binding_id = ?`, binding)
	if !h.startUnseen("reader") {
		t.Fatal("a launch of an earlier selection hid the unseen start")
	}
	// An adapter that cannot send a notice has no route to explain.
	h.sql(`DELETE FROM link_event_launch WHERE principal_id = ?`, h.principal)
	h.sql(`UPDATE device_key SET vendor = 'agy' WHERE principal_id = ?`, h.principal)
	if h.startUnseen("reader") {
		t.Fatal("a linked member whose adapter has no sender shows an unseen start")
	}
}
