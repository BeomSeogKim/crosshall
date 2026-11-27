package dispatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/hostclock"
	"github.com/BeomSeogKim/crosshall/internal/link"
	"github.com/BeomSeogKim/crosshall/internal/proc"
	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

type spokeNativeRoute struct {
	Member      link.Member
	Boot        string
	Scope       link.MemberScope
	Launch      string
	PolicyEpoch int64
	CaptureSeq  uint64
	Probe       publish.Probe
}

type spokeNativeHook struct {
	enroll       bool
	fact         string
	mode         string
	startedAtMS  int64
	hookAtMS     int64
	verified     bool
	pidNamespace string
	host         *wire.Host
	vendorHome   string
	report       *wire.HookRouteArgs
}

type spokeNativeMaterial struct {
	Route        publish.Route
	StartedAtMS  int64
	PIDNamespace string
	Verified     bool
	Ended        bool
	BusyUntilNS  int64
}

type spokeNativeEvidence struct {
	Material      spokeNativeMaterial
	ProfileSeq    uint64
	SocketSeq     uint64
	PermissionSeq uint64
	ActivitySeq   uint64
}

func recordSpokeNativeHook(ctx context.Context, tx store.Tx, member link.Member, token link.HookToken, launch string, epoch uint64,
	hook spokeNativeHook,
) error {
	adapter, ok := publish.Lookup(member.Vendor)
	if !ok || adapter.Transport().Send == nil || launch == "" || epoch > math.MaxInt64 || token.LeafSeq > math.MaxInt64 {
		return nil
	}
	// The turn end or idle prompt of a process launch carries the launch's
	// time as a prompt does, so a session that joined during a turn has a
	// route without a new prompt (#437). A thread-routed adapter's route
	// rests on the server its hooks name, which a turn end names too.
	threadTurnEnd := adapter.ThreadRouted() && hook.fact == wire.HookTurnFinishing && hook.host != nil
	profile := hook.enroll || hook.fact == wire.HookPromptSubmitted || threadTurnEnd ||
		(hook.fact == wire.HookTurnFinishing || hook.fact == wire.HookIdlePrompt) && hook.verified && hook.startedAtMS > 0 &&
			!adapter.ThreadRouted()
	if !profile && hook.report == nil {
		switch hook.fact {
		case wire.HookApprovalRequested, wire.HookTurnFinishing, wire.HookIdlePrompt, wire.HookSessionEnded:
		default:
			return nil
		}
	}
	var routeEpoch int64
	var cutSeq uint64
	err := tx.QueryRowContext(ctx, `SELECT route_epoch, cut_seq FROM spoke_member_scope
		WHERE vendor = ? AND key = ? AND leaf_generation = ? AND binding_id = ? AND membership_gen = ?`,
		member.Vendor, member.Key, token.LeafGeneration, token.Membership.BindingID, token.Membership.Generation).
		Scan(&routeEpoch, &cutSeq)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if routeEpoch != 0 && token.LeafSeq <= cutSeq {
		return nil
	}
	var old spokeNativeRoute
	var body string
	err = tx.QueryRowContext(ctx, `SELECT boot, leaf_generation, binding_id, membership_gen, route_epoch, launch, policy_epoch, capture_seq, evidence
		FROM spoke_native_route WHERE vendor = ? AND key = ?`, member.Vendor, member.Key).
		Scan(&old.Boot, &old.Scope.LeafGeneration, &old.Scope.Membership.BindingID, &old.Scope.Membership.Generation,
			&old.Scope.RouteEpoch, &old.Launch, &old.PolicyEpoch, &old.CaptureSeq, &body)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var evidence spokeNativeEvidence
	sameLeaf := err == nil && old.Scope.LeafGeneration == token.LeafGeneration
	sameEpoch := old.Scope.RouteEpoch == routeEpoch || old.Scope.RouteEpoch == 0 && old.CaptureSeq > cutSeq
	sameScope := sameLeaf && old.Boot == token.Boot && old.Scope.Membership == token.Membership &&
		old.PolicyEpoch == int64(epoch) && sameEpoch
	if sameLeaf {
		if err := json.Unmarshal([]byte(body), &evidence); err != nil {
			return err
		}
		// Only a prompt or a start moves a scope's route from one verified
		// launch to another: the home keeps offering for the launch it
		// opened, which a turn end of another process does not replace.
		if profile && !hook.enroll && hook.fact != wire.HookPromptSubmitted && sameScope && old.Launch != launch &&
			evidence.Material.Verified {
			profile = false
		}
		// A turn end gives a thread its server only when no usable one is
		// named yet: a start or a prompt says which server the session runs
		// in now. A server in another PID namespace, which the reader never
		// offers, does not count.
		if threadTurnEnd && sameScope && old.Launch == launch && evidence.Material.Route.Host.PID > 0 &&
			evidence.Material.PIDNamespace == proc.PIDNamespace() {
			profile = false
		}
		if (!sameScope || old.Launch != launch) &&
			token.LeafSeq <= max(evidence.ProfileSeq, evidence.SocketSeq, evidence.PermissionSeq, evidence.ActivitySeq) {
			return nil
		}
	}
	if !sameScope || old.Launch != launch {
		if !profile && (sameScope || hook.report == nil) {
			return nil
		}
		evidence = spokeNativeEvidence{}
	}

	before := evidence.Material
	evidence.apply(adapter, member.Key, token, hook, profile)
	captureSeq := old.CaptureSeq
	if !sameScope {
		captureSeq = token.LeafSeq
	} else if old.Launch != launch || evidence.Material != before {
		captureSeq = max(captureSeq, token.LeafSeq)
	}
	raw, err := json.Marshal(evidence)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO spoke_native_route
		(vendor, key, boot, leaf_generation, binding_id, membership_gen, route_epoch, launch, policy_epoch, capture_seq, evidence)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (vendor, key) DO UPDATE SET boot = excluded.boot, leaf_generation = excluded.leaf_generation,
			binding_id = excluded.binding_id, membership_gen = excluded.membership_gen, route_epoch = excluded.route_epoch, launch = excluded.launch,
			policy_epoch = excluded.policy_epoch, capture_seq = excluded.capture_seq, evidence = excluded.evidence`,
		member.Vendor, member.Key, token.Boot, token.LeafGeneration, token.Membership.BindingID,
		token.Membership.Generation, routeEpoch, launch, epoch, captureSeq, string(raw))
	return err
}

// spokeNativeLaunch is the launch of the member's native route in the
// token's scope: a route report for another launch in that scope is not
// applied.
func spokeNativeLaunch(ctx context.Context, tx store.Tx, member link.Member, token link.HookToken) (string, error) {
	var launch string
	err := tx.QueryRowContext(ctx, `SELECT launch FROM spoke_native_route WHERE vendor = ? AND key = ? AND boot = ?
		AND leaf_generation = ? AND binding_id = ? AND membership_gen = ?`, member.Vendor, member.Key, token.Boot,
		token.LeafGeneration, token.Membership.BindingID, token.Membership.Generation).Scan(&launch)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return launch, err
}

// spokeThreadServerRouted reports whether a thread-routed member's route in
// the token's scope names a managed server of this PID namespace for the
// launch: what a turn end must leave before the home opens the launch at it
// (#437).
func spokeThreadServerRouted(ctx context.Context, tx store.Tx, member link.Member, token link.HookToken, launch string, epoch uint64) (bool, error) {
	adapter, ok := publish.Lookup(member.Vendor)
	if !ok || !adapter.ThreadRouted() || epoch > math.MaxInt64 {
		return false, nil
	}
	var body string
	err := tx.QueryRowContext(ctx, `SELECT evidence FROM spoke_native_route WHERE vendor = ? AND key = ? AND boot = ?
		AND leaf_generation = ? AND binding_id = ? AND membership_gen = ? AND launch = ? AND policy_epoch = ?`,
		member.Vendor, member.Key, token.Boot, token.LeafGeneration, token.Membership.BindingID, token.Membership.Generation,
		launch, int64(epoch)).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var evidence spokeNativeEvidence
	if err := json.Unmarshal([]byte(body), &evidence); err != nil {
		return false, err
	}
	m := evidence.Material
	return m.Route.Mode == publish.ModeDaemon && m.Route.Host.PID > 0 && m.PIDNamespace == proc.PIDNamespace(), nil
}

func (e *spokeNativeEvidence) apply(adapter publish.Adapter, key string, token link.HookToken, hook spokeNativeHook, profile bool) {
	if profile && token.LeafSeq > e.ProfileSeq {
		e.ProfileSeq = token.LeafSeq
		startedAtMS := hook.startedAtMS
		if adapter.ThreadRouted() && startedAtMS <= 0 {
			// A thread can recover at a prompt even when its hooks cannot
			// bind a process launch of their own.
			startedAtMS = hook.hookAtMS
		}
		if startedAtMS > 0 && (e.Material.StartedAtMS == 0 || startedAtMS < e.Material.StartedAtMS) {
			e.Material.StartedAtMS = startedAtMS
		}
		if adapter.ThreadRouted() {
			previousHost, previousHome := e.Material.Route.Host, e.Material.Route.Home
			route := publish.Route{Mode: hook.mode, Home: hook.vendorHome}
			e.Material.PIDNamespace = ""
			if hook.host != nil {
				route.Host = publish.Queue{PID: hook.host.PID, StartMS: hook.host.Start}
				e.Material.PIDNamespace = hook.host.NS
			}
			e.Material.Route = adapter.SessionRoute(key, route)
			if e.Material.Route.Host != previousHost || e.Material.Route.Home != previousHome {
				e.Material.StartedAtMS = startedAtMS
			}
		} else if hook.verified {
			e.Material.Verified, e.Material.PIDNamespace = true, hook.pidNamespace
		}
	}
	if report := hook.report; report != nil && !adapter.ThreadRouted() {
		route := &e.Material.Route
		if token.LeafSeq > e.SocketSeq && adapter.AcceptsSocket(report.SocketPath) && (route.SocketPath == "" || report.Start) {
			route.SocketPath = report.SocketPath
			e.SocketSeq = token.LeafSeq
		}
		if token.LeafSeq > e.PermissionSeq && report.PermissionMode != "" {
			route.Mode = report.PermissionMode
			e.PermissionSeq = token.LeafSeq
		}
	}
	if hook.report != nil || token.LeafSeq <= e.ActivitySeq {
		return
	}
	e.ActivitySeq = token.LeafSeq
	switch {
	case hook.enroll:
		e.Material.Ended, e.Material.BusyUntilNS = false, 0
	case hook.fact == wire.HookSessionEnded:
		e.Material.Ended = true
	case hook.fact == wire.HookPromptSubmitted || hook.fact == wire.HookApprovalRequested:
		e.Material.BusyUntilNS = min(token.MonoNS, math.MaxInt64-int64(30*time.Minute)) + int64(30*time.Minute)
	case hook.fact == wire.HookTurnFinishing || hook.fact == wire.HookIdlePrompt:
		e.Material.BusyUntilNS = 0
	}
}

const spokeNativeRoutesSQL = `SELECT r.vendor, r.key, r.boot, r.leaf_generation, r.binding_id,
		r.membership_gen, s.route_epoch, r.launch, r.policy_epoch, r.capture_seq, r.evidence
	FROM spoke_native_route r
	JOIN spoke_leaf l ON l.vendor = r.vendor AND l.key = r.key AND l.generation = r.leaf_generation
		AND l.binding_id = r.binding_id AND l.membership_gen = r.membership_gen
	JOIN spoke_member_scope s ON s.vendor = r.vendor AND s.key = r.key AND s.leaf_generation = r.leaf_generation
		AND s.binding_id = r.binding_id AND s.membership_gen = r.membership_gen
		AND (s.route_epoch = r.route_epoch OR r.route_epoch = 0 AND r.capture_seq > s.cut_seq)
	JOIN spoke_policy p ON p.adapter = r.vendor AND p.epoch = r.policy_epoch AND p.enabled = 1
	JOIN spoke_event_cursor c ON c.id = 1 AND c.boot = r.boot AND c.eligible = 1 AND c.rolled_back = 0
	WHERE r.boot = ? AND s.route_epoch > 0 AND r.policy_epoch > 0 AND r.capture_seq > 0`

func readSpokeNativeRoutes(ctx context.Context, tx store.Tx, boot string, mono int64) ([]spokeNativeRoute, error) {
	return querySpokeNativeRoutes(ctx, tx, spokeNativeRoutesSQL+`
		AND EXISTS (SELECT 1 FROM spoke_offer o WHERE o.vendor = r.vendor AND o.key = r.key
			AND o.boot = r.boot AND o.suppressed = 0 AND o.deadline_mono > ?
			AND json_extract(o.body, '$.leaf_generation') = r.leaf_generation
			AND json_extract(o.body, '$.membership.binding_id') = r.binding_id
			AND json_extract(o.body, '$.membership.generation') = r.membership_gen
			AND json_extract(o.body, '$.route_epoch') = s.route_epoch
			AND json_extract(o.body, '$.launch') = r.launch
			AND json_extract(o.body, '$.policy_epoch') = r.policy_epoch
			AND json_array_length(o.body, '$.ids') > 0)
		ORDER BY r.vendor, r.key`, boot, mono)
}

func readSpokeNativeRoute(ctx context.Context, tx store.Tx, boot string, member link.Member) (spokeNativeRoute, bool, error) {
	routes, err := querySpokeNativeRoutes(ctx, tx, spokeNativeRoutesSQL+` AND r.vendor = ? AND r.key = ?`, boot, member.Vendor, member.Key)
	if err != nil || len(routes) == 0 {
		return spokeNativeRoute{}, false, err
	}
	return routes[0], true, nil
}

func querySpokeNativeRoutes(ctx context.Context, tx store.Tx, query string, args ...any) ([]spokeNativeRoute, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var routes []spokeNativeRoute
	var mono int64
	var namespace string
	for rows.Next() {
		if mono == 0 {
			if mono, err = hostclock.Mono(); err != nil {
				return nil, err
			}
			namespace = proc.PIDNamespace()
		}
		var r spokeNativeRoute
		var body string
		if err := rows.Scan(&r.Member.Vendor, &r.Member.Key, &r.Boot, &r.Scope.LeafGeneration, &r.Scope.Membership.BindingID,
			&r.Scope.Membership.Generation, &r.Scope.RouteEpoch, &r.Launch, &r.PolicyEpoch, &r.CaptureSeq, &body); err != nil {
			return nil, err
		}
		var evidence spokeNativeEvidence
		if err := json.Unmarshal([]byte(body), &evidence); err != nil {
			return nil, err
		}
		m := evidence.Material
		adapter, ok := publish.Lookup(r.Member.Vendor)
		if !ok || m.Ended || m.BusyUntilNS > mono || m.PIDNamespace != namespace || m.StartedAtMS <= 0 ||
			!adapter.ThreadRouted() && !m.Verified {
			continue
		}
		address, why := publish.Adapters().Address(r.Member.Vendor, m.Route)
		if address == "" || why != "" {
			continue
		}
		r.Probe = publish.Probe{Target: publish.Target{Adapter: r.Member.Vendor, Address: address},
			Route: m.Route, LaunchRef: r.Launch, StartedHookAtMS: m.StartedAtMS}
		routes = append(routes, r)
	}
	return routes, rows.Err()
}
