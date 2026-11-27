package domain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/link"
	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

const (
	VerbLinkWakeReady  = "owner link wake ready"
	VerbLinkWakeResult = "owner link wake result"
	wakeDispatching    = "dispatching"
)

type LinkWakeReady struct {
	LinkEventPeer
	Route link.WakeRoute `json:"route"`
}

type LinkWakeReadyResult struct {
	Seq    uint64     `json:"seq,omitempty"`
	Replay *link.Wake `json:"replay,omitempty"`
}

type LinkWakeResult struct {
	LinkEventPeer
	Result link.WakeResult `json:"result"`
}

func (LinkWakeReady) command()  {}
func (LinkWakeResult) command() {}

func (LinkWakeReady) Verb() string  { return VerbLinkWakeReady }
func (LinkWakeResult) Verb() string { return VerbLinkWakeResult }

func validWakeRoute(route link.WakeRoute) bool {
	return ValidName(route.Vendor) && route.Key != "" && len(route.Key) <= 4096 &&
		route.Boot != "" && validRef(route.Boot) && route.Launch != "" && validRef(route.Launch) &&
		route.Scope.LeafGeneration != "" && validRef(route.Scope.LeafGeneration) &&
		route.Scope.Membership.BindingID > 0 && route.Scope.Membership.Generation > 0 &&
		route.Scope.RouteEpoch > 0 && route.PolicyEpoch > 0 && link.ValidNonce(route.Lane)
}

func authorizeLinkWake(ctx context.Context, tx store.Tx, caller core.Caller, peer LinkEventPeer) (linkEventStream, *core.Result, error) {
	if refusal, err := authorizeLinkEventPeer(ctx, tx, caller, peer); refusal != nil || err != nil {
		return linkEventStream{}, refusal, err
	}
	stream, err := readLinkEventStream(ctx, tx, peer)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !stream.wakeCapable {
		result := refuseLinkEvents("wakes require a subscribed wake-capable link")
		return linkEventStream{}, &result, nil
	}
	return stream, nil, err
}

func (s *Service) linkWakeReady(ctx context.Context, tx store.Tx, caller core.Caller, cmd LinkWakeReady, now time.Time) (core.Result, error) {
	peer, route := cmd.LinkEventPeer, cmd.Route
	stream, refusal, err := authorizeLinkWake(ctx, tx, caller, peer)
	if refusal != nil {
		return *refusal, nil
	}
	if err != nil {
		return core.Result{}, err
	}
	if !validWakeRoute(route) {
		return Fail(core.ExitUsage, "", "invalid linked wake route", "check the linked device's local notice route"), nil
	}
	if err := expireHeldLinkWakes(ctx, tx, peer.Device, now); err != nil {
		return core.Result{}, err
	}
	if stream.snapshotSeq.Valid || route.Boot != stream.boot {
		return OK(LinkWakeReadyResult{})
	}
	cursor, err := readLinkEventCursor(ctx, tx, peer.Device)
	if err != nil {
		return core.Result{}, err
	}
	if cursor.boot != route.Boot || cursor.credentialGen != peer.CredentialGen || cursor.trimmed > cursor.acked {
		return OK(LinkWakeReadyResult{})
	}
	scope, err := readLinkOfferScope(ctx, tx, peer.Device, route.Member)
	if err != nil {
		return core.Result{}, err
	}
	if !scope.live() || scope.binding.dnd || scope.MemberScope != route.Scope {
		return OK(LinkWakeReadyResult{})
	}
	if ready, err := linkWakeRouteEligible(ctx, tx, peer, scope.principal, route); err != nil {
		return core.Result{}, err
	} else if !ready {
		return OK(LinkWakeReadyResult{})
	}
	if running, err := inReportedTurn(ctx, tx, scope.principal, now); err != nil {
		return core.Result{}, err
	} else if running {
		return OK(LinkWakeReadyResult{})
	}
	raw, err := json.Marshal(route)
	if err != nil {
		return core.Result{}, err
	}
	ids, err := pendingLinkWakeIDs(ctx, tx, scope, string(raw))
	if err != nil {
		return core.Result{}, err
	}
	if len(ids) == 0 {
		return OK(LinkWakeReadyResult{})
	}
	existing, found, err := replayLinkWake(ctx, tx, peer, scope.principal, route, string(raw), ids)
	if err != nil {
		return core.Result{}, err
	}
	if found {
		return OK(existing)
	}
	return dispatchLinkWake(ctx, tx, peer, scope, route, string(raw), ids, now)
}

func linkWakeRouteEligible(ctx context.Context, tx store.Tx, peer LinkEventPeer, principal int64, route link.WakeRoute) (bool, error) {
	adapter, ok := publish.Lookup(route.Vendor)
	if !ok || adapter.Transport().Send == nil {
		return false, nil
	}
	var ready bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM link_event_launch l
		JOIN adapter_launch a ON a.principal_id = l.principal_id AND a.launch_ref = l.projection_ref
		JOIN binding b ON b.id = l.binding_id AND b.principal_id = l.principal_id
		JOIN adapter_policy policy ON policy.adapter = ?1 AND policy.epoch = l.policy_epoch
		JOIN link_event_cut cut ON cut.device_id = l.device_id AND cut.vendor = ?1 AND cut.key = ?2
		WHERE l.principal_id = ?3 AND l.device_id = ?4 AND l.credential_gen = ?5 AND l.boot = ?6
			AND l.launch_ref = ?7 AND l.policy_epoch = ?8 AND l.leaf_generation = ?9
			AND l.binding_id = ?10 AND l.membership_generation = ?11 AND l.route_epoch = ?12
			AND a.ended_hook_at_ms IS NULL AND a.fenced = 0 AND b.state = 'live'
			AND cut.boot = l.boot AND cut.leaf_generation = l.leaf_generation
			AND cut.binding_id = l.binding_id AND cut.membership_generation = l.membership_generation
			AND cut.route_epoch = l.route_epoch AND cut.absent = 0
			AND NOT EXISTS (SELECT 1 FROM device_offer_dirty dirty
				WHERE dirty.device_id = l.device_id AND dirty.vendor = ?1 AND dirty.key = ?2
					AND (dirty.membership = 1 OR dirty.invalidate_reason IN ('move', 'depart', 'route', 'policy')))
			AND NOT EXISTS (SELECT 1 FROM device_event e WHERE e.device_id = l.device_id AND e.seq > cut.event_seq
				AND (e.kind = 'membership' OR (e.kind = 'invalidate' AND json_extract(e.body, '$.reason') IN ('move', 'depart', 'route', 'policy')))
				AND json_extract(e.body, '$.vendor') = ?1 AND json_extract(e.body, '$.key') = ?2)
			AND (CASE WHEN ?13 THEN json_extract(a.profile, '$.mode') = 'daemon' ELSE l.verified = 1 END)
			AND `+store.PublishSwitchesOn+` AND `+store.CapabilityCovered("?1", "a.profile", "'publish'")+`
	)`, route.Vendor, route.Key, principal, peer.Device, peer.CredentialGen, route.Boot, route.Launch,
		route.PolicyEpoch, route.Scope.LeafGeneration, route.Scope.Membership.BindingID,
		route.Scope.Membership.Generation, route.Scope.RouteEpoch, adapter.ThreadRouted()).Scan(&ready)
	return ready, err
}

// linkWakeOnRoute matches an attempt a made on a member's linked route
// (boot, launch, policy epoch and scope, in that order), whatever lane the
// device chose for it.
const linkWakeOnRoute = `json_extract(a.route, '$.boot') = ? AND json_extract(a.route, '$.launch') = ?
	AND json_extract(a.route, '$.policy_epoch') = ? AND json_extract(a.route, '$.scope.leaf_generation') = ?
	AND json_extract(a.route, '$.scope.membership.binding_id') = ? AND json_extract(a.route, '$.scope.membership.generation') = ?
	AND json_extract(a.route, '$.scope.route_epoch') = ?`

// An attempt that may have submitted keeps its original IDs out of later wakes.
const linkWakeMayHaveSubmitted = `(a.state IN ('submitted', 'uncertain')
	OR (a.state = 'dispatching' AND a.released_at_ms IS NOT NULL))`

func pendingLinkWakeIDs(ctx context.Context, tx store.Tx, scope linkOfferScope, route string) ([]int64, error) {
	return selectIDs(ctx, tx, `SELECT x.message_id FROM recipient x JOIN message m ON m.id = x.message_id
		WHERE x.principal_id = ?1 AND m.channel_id = ?2 AND `+pendingPublishItem+`
		AND NOT EXISTS (SELECT 1 FROM announcement a WHERE a.principal_id = x.principal_id AND a.message_id = x.message_id)
		AND NOT EXISTS (SELECT 1 FROM link_wake_attempt a, json_each(a.message_ids) item
			WHERE a.principal_id = ?1 AND item.value = x.message_id
				AND (`+linkWakeMayHaveSubmitted+`
					OR (a.state = 'not_submitted' AND a.submission_seq > 0 AND a.route = ?3)))
		ORDER BY x.message_id LIMIT ?4`, scope.principal, scope.binding.channel, route, OfferCap)
}

func linkedMemberWake(ctx context.Context, tx store.Tx, binding int64, now time.Time) (Wake, error) {
	none := Wake{Wake: WakeNone, Why: WhyNoRoute}
	var peer LinkEventPeer
	var route link.WakeRoute
	var closed bool
	var principal int64
	err := tx.QueryRowContext(ctx, `SELECT l.device_id, l.credential_gen, k.vendor, k.key,
			l.boot, l.launch_ref, l.policy_epoch, l.leaf_generation, l.membership_generation, l.route_epoch,
			l.principal_id, s.closed_at_ms IS NOT NULL
		FROM binding b JOIN link_event_launch l ON l.principal_id = b.principal_id AND l.binding_id = b.id
		JOIN adapter_launch a ON a.principal_id = l.principal_id AND a.launch_ref = l.projection_ref
		JOIN device_key k ON k.principal_id = l.principal_id AND k.device_id = l.device_id
			AND k.credential_gen = l.credential_gen AND k.leaf_generation = l.leaf_generation
			AND k.vendor = json_extract(a.profile, '$.adapter') AND k.state = 'live'
		JOIN link_event_stream s ON s.device_id = l.device_id AND s.boot = l.boot AND s.wake_capable = 1
		JOIN link_event_cursor c ON c.device_id = l.device_id AND c.boot = l.boot AND c.credential_gen = l.credential_gen
		WHERE b.id = ? AND s.snapshot_seq IS NULL`, binding).
		Scan(&peer.Device, &peer.CredentialGen, &route.Vendor, &route.Key,
			&route.Boot, &route.Launch, &route.PolicyEpoch, &route.Scope.LeafGeneration,
			&route.Scope.Membership.Generation, &route.Scope.RouteEpoch, &principal, &closed)
	if errors.Is(err, sql.ErrNoRows) {
		return none, nil
	}
	if err != nil {
		return none, err
	}
	route.Scope.Membership.BindingID = binding
	scope, err := readLinkOfferScope(ctx, tx, peer.Device, route.Member)
	if err != nil || !scope.live() || scope.credential != peer.CredentialGen || scope.MemberScope != route.Scope {
		return none, err
	}
	var held bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM link_wake_attempt
		WHERE device_id = ? AND principal_id = ? AND binding_id = ? AND released_at_ms IS NULL
		AND (state = 'dispatching' OR (state IN ('submitted', 'uncertain') AND first_result_at_ms > ?)))`,
		peer.Device, principal, binding, ms(now.Add(-linkWakeHeldWindow))).Scan(&held); err != nil {
		return none, err
	}
	if held {
		return Wake{Wake: WakeNone, Why: WhyHeld}, nil
	}
	if scope.binding.dnd {
		return none, nil
	}
	eligible, err := linkWakeRouteEligible(ctx, tx, peer, principal, route)
	if err != nil || !eligible {
		return none, err
	}
	if !closed {
		// Only the device can prove its session's route, and it does so
		// when it delivers a wake: a launch whose latest wake on this route
		// was submitted is a notice route (#438), like a local member's
		// available publish state. Until then the home cannot tell.
		var proven bool
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT a.state = 'submitted' FROM link_wake_attempt a
			WHERE a.device_id = ? AND a.credential_gen = ? AND a.principal_id = ? AND `+linkWakeOnRoute+`
			ORDER BY a.id DESC LIMIT 1), 0)`, peer.Device, peer.CredentialGen, principal,
			route.Boot, route.Launch, route.PolicyEpoch, route.Scope.LeafGeneration, route.Scope.Membership.BindingID,
			route.Scope.Membership.Generation, route.Scope.RouteEpoch).Scan(&proven); err != nil || !proven {
			return none, err
		}
		return Wake{Wake: WakeNotice}, nil
	}
	running, err := inReportedTurn(ctx, tx, principal, now)
	if err != nil || running {
		return none, err
	}
	return Wake{Wake: WakeNone, Why: WhyDeviceOffline}, nil
}

func replayLinkWake(ctx context.Context, tx store.Tx, peer LinkEventPeer, principal int64, route link.WakeRoute, raw string,
	eligible []int64) (LinkWakeReadyResult, bool, error) {
	var wake link.Wake
	var state, original, ids string
	var credential int64
	var seq uint64
	err := tx.QueryRowContext(ctx, `SELECT id, credential_gen, route, message_ids, event_seq, state FROM link_wake_attempt
		WHERE device_id = ? AND (principal_id = ? OR (vendor = ? AND lane = ?))
			AND state <> 'not_submitted' AND released_at_ms IS NULL ORDER BY id LIMIT 1`,
		peer.Device, principal, route.Vendor, route.Lane).Scan(&wake.AttemptID, &credential, &original, &ids, &seq, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return LinkWakeReadyResult{}, false, nil
	}
	if err != nil {
		return LinkWakeReadyResult{}, false, err
	}
	if state != wakeDispatching || credential != peer.CredentialGen || original != raw {
		return LinkWakeReadyResult{}, true, nil
	}
	if err := json.Unmarshal([]byte(ids), &wake.IDs); err != nil {
		return LinkWakeReadyResult{}, false, err
	}
	for _, id := range wake.IDs {
		if !slices.Contains(eligible, id) {
			return LinkWakeReadyResult{}, true, nil
		}
	}
	wake.Route = route
	return LinkWakeReadyResult{Seq: seq, Replay: &wake}, true, nil
}

func dispatchLinkWake(ctx context.Context, tx store.Tx, peer LinkEventPeer, scope linkOfferScope, route link.WakeRoute,
	raw string, ids []int64, now time.Time) (core.Result, error) {
	data, err := json.Marshal(ids)
	if err != nil {
		return core.Result{}, err
	}
	wake := link.Wake{Route: route, IDs: ids}
	err = tx.QueryRowContext(ctx, `INSERT INTO link_wake_attempt
		(device_id, credential_gen, principal_id, binding_id, vendor, lane, route, message_ids, state, dispatched_at_ms)
		SELECT ?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, 'dispatching', ?9
		WHERE (SELECT COUNT(*) FROM link_wake_attempt a WHERE a.principal_id = ?3 AND a.dispatched_at_ms > ?10) < ?11
		ON CONFLICT DO NOTHING RETURNING id`, peer.Device, peer.CredentialGen, scope.principal, scope.Membership.BindingID,
		route.Vendor, route.Lane, raw, string(data), ms(now), ms(now.Add(-time.Hour)), store.PublishHourlyCap).Scan(&wake.AttemptID)
	if errors.Is(err, sql.ErrNoRows) {
		return OK(LinkWakeReadyResult{})
	}
	if err != nil {
		return core.Result{}, err
	}
	seq, err := appendLinkEvent(ctx, tx, peer.Device, link.EventWake, wake, now)
	if err != nil {
		return core.Result{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE link_wake_attempt SET event_seq = ? WHERE id = ?`, seq, wake.AttemptID); err != nil {
		return core.Result{}, err
	}
	return OK(LinkWakeReadyResult{Seq: seq})
}

func (s *Service) linkWakeResult(ctx context.Context, tx store.Tx, caller core.Caller, cmd LinkWakeResult, now time.Time) (core.Result, error) {
	peer, result := cmd.LinkEventPeer, cmd.Result
	_, refusal, err := authorizeLinkWake(ctx, tx, caller, peer)
	if refusal != nil {
		return *refusal, nil
	}
	if err != nil {
		return core.Result{}, err
	}
	rejected := link.WakeRecorded{AttemptID: result.AttemptID, Status: link.RecordedStale, Code: "invalid_input"}
	if result.AttemptID <= 0 || !validWakeRoute(result.Route) || !validLinkInteger(result.SubmissionSeq) ||
		(result.Outcome != link.WakeNotSubmitted && result.SubmissionSeq == 0) {
		return OK(rejected)
	}
	switch result.Outcome {
	case link.WakeNotSubmitted, link.WakeSubmitted, link.WakeUncertain:
	default:
		return OK(rejected)
	}
	if err := expireHeldLinkWakes(ctx, tx, peer.Device, now); err != nil {
		return core.Result{}, err
	}
	raw, err := json.Marshal(result.Route)
	if err != nil {
		return core.Result{}, err
	}
	var state string
	var seq uint64
	err = tx.QueryRowContext(ctx, `SELECT state, submission_seq FROM link_wake_attempt
		WHERE id = ? AND device_id = ? AND credential_gen <= ? AND route = ?`,
		result.AttemptID, peer.Device, peer.CredentialGen, string(raw)).Scan(&state, &seq)
	if errors.Is(err, sql.ErrNoRows) {
		rejected.Code = "wake_scope"
		return OK(rejected)
	}
	if err != nil {
		return core.Result{}, err
	}
	if (seq > 0 && result.SubmissionSeq > 0 && seq != result.SubmissionSeq) ||
		(state != wakeDispatching && state != link.WakeUncertain && state != result.Outcome) {
		rejected.Code = string(core.CodeKeyConflict)
		return OK(rejected)
	}
	if state != result.Outcome || seq == 0 && result.SubmissionSeq > 0 {
		_, err := tx.ExecContext(ctx, `UPDATE link_wake_attempt SET state = ?1, submission_seq = max(submission_seq, ?2),
			released_at_ms = CASE WHEN ?1 IN ('submitted', 'uncertain') AND max(submission_seq, ?2) > 0
				AND activity_seq > max(submission_seq, ?2) THEN COALESCE(released_at_ms, ?3) ELSE released_at_ms END,
			first_result_at_ms = COALESCE(first_result_at_ms, ?3) WHERE id = ?4`, result.Outcome, result.SubmissionSeq, ms(now), result.AttemptID)
		if err != nil {
			return core.Result{}, err
		}
	}
	return OK(link.WakeRecorded{AttemptID: result.AttemptID, Status: link.RecordedApplied})
}
