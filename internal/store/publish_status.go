package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/publish"
)

// PublishNotActivated is the candidate reason of a proven route that has
// not been available since its proof: every other check passes, so the
// publisher activates it, and sets its watermark, on its next tick.
const PublishNotActivated = "route not yet activated"

// PublishNoSender begins the candidate reason of a route whose adapter has
// no sender; the adapter's name follows it.
const PublishNoSender = "no sender for "

// PublishHourlyCap bounds the notices one member can be offered in an hour.
const PublishHourlyCap = 30

const PublishSuspensionDuration = 30 * time.Minute

// Older dispatchers could make an unknown receipt's suspension last for the
// launch. Give those rows a deadline without changing the recorded history.
var publishSuspensionExpiry = fmt.Sprintf(`CASE WHEN s.reason = 'unknown' AND s.until_ms IS NULL
	THEN s.at_ms + %d ELSE s.until_ms END`, PublishSuspensionDuration.Milliseconds())

// Marking raises the notice stamp once. A marked hint is still offered until
// the hook acknowledges printing it.
const (
	RecoveryHintPending = iota
	RecoveryHintMarked
	RecoveryHintAcknowledged
)

type PublishRecoveryHint struct {
	SuspensionID int64
	State        int
}

// PublishSwitchesOn: the adapter is enabled, the narrowest owner switch
// set (member, then adapter, then hall) is on or none is set, and the
// member has not turned publish off for itself.
const PublishSwitchesOn = `policy.enabled = 1 AND b.publish_quiet = 0
	AND COALESCE(b.publish_owner, policy.publish, (SELECT h.enabled FROM hall_publish h WHERE h.id = 1), 1) = 1`

var PublishProfileCovered = CapabilityCovered("r.adapter", "l.profile", "'publish'")

// PublishAdapterSends holds when the route's adapter can publish: it has a
// notice format and a transport that sends. Whether the route itself names
// an address is the adapter's to resolve (publish.Registry.Address).
func PublishAdapterSends() string { return adapterIn("r.adapter", publish.Adapters().Senders()) }

func adapterIn(column string, names []string) string {
	if len(names) == 0 {
		return "0"
	}
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = "'" + strings.ReplaceAll(name, "'", "''") + "'"
	}
	return column + " IN (" + strings.Join(quoted, ", ") + ")"
}

// ThreadRoutedAdapter holds when the adapter named by column addresses a
// session's thread on the server that hosts it (publish.Adapter.ThreadRouted):
// its launches are never verified ones.
func ThreadRoutedAdapter(column string) string {
	var names []string
	registry := publish.Adapters()
	for _, name := range registry.Names() {
		if registry[name].ThreadRouted() {
			names = append(names, name)
		}
	}
	return adapterIn(column, names)
}

// PublishThreadRouted holds when route r is thread-routed and names its
// thread, its server and that server's home.
func PublishThreadRouted() string {
	return "(" + ThreadRoutedAdapter("r.adapter") + ` AND r.thread_id IS NOT NULL AND r.host_pid IS NOT NULL
		AND r.host_start IS NOT NULL AND r.vendor_home IS NOT NULL)`
}

// PublishLaunchBinds holds when the current launch l may publish on route
// r: it is the route's launch, and either both are verified or the route
// is thread-routed, so its transport proves the hosting server instead.
func PublishLaunchBinds() string {
	return "(l.launch_ref = r.launch_ref AND l.fenced = 0 AND " + PublishLaunchAfterOwnerFence +
		" AND (r.launch_verified = 1 AND l.verified = 1 OR " + PublishThreadRouted() + "))"
}

const PublishLaunchAfterOwnerFence = "(b.owner_hook_fence_ms IS NULL OR l.started_hook_at_ms > b.owner_hook_fence_ms)"

// PublishLane is the queue route r's notices hold once proven by p, which
// one open notice at a time may use (publish_attempt.lane): the thread of
// a thread-routed route in its home, whose server hosts other threads too
// and whose thread IDs are the home's own, else the proven process.
func PublishLane() string {
	return "CASE WHEN " + PublishThreadRouted() + " THEN " + PublishThreadLane() + `
		ELSE 'process:' || p.process_pid || ':' || p.process_start_ms END`
}

// PublishThreadLane is the lane of thread-routed route r: its thread in its home.
func PublishThreadLane() string {
	return `('thread:' || r.adapter || ':' || r.thread_id || ':' || r.vendor_home)`
}

// PublishNotSuspended holds when no suspension of the route's launch,
// committed after the owner's last resume, is still in force at nowMS.
func PublishNotSuspended(nowMS int64) string {
	return fmt.Sprintf(`NOT EXISTS (SELECT 1 FROM publish_suspension s
		WHERE s.binding_id IN (SELECT id FROM binding WHERE principal_id = r.principal_id)
		AND s.launch_ref = r.launch_ref AND s.id > b.publish_resume_suspension
		AND COALESCE((%s) > %d, 1))`, publishSuspensionExpiry, nowMS)
}

// PublishState is one member's publish state as who and doctor show it.
// State is off, candidate, available, held, suspended or capped. Notice is
// the attempt a held or suspended state refers to; AtMS is when it was
// held or suspended, UntilMS when a cap or a timed suspension ends. Lift
// says how a suspension ends. Agents see State only.
type PublishState struct {
	State        string              `json:"state"`
	Reason       string              `json:"reason,omitempty"`
	Notice       int64               `json:"notice,omitempty"`
	AtMS         int64               `json:"at_ms,omitempty"`
	UntilMS      int64               `json:"until_ms,omitempty"`
	Lift         string              `json:"lift,omitempty"`
	Generation   int64               `json:"generation,omitempty"`
	RecoveryHint PublishRecoveryHint `json:"-"`
	// Invalidated marks an off state that is the route's own: it cannot
	// carry a notice, and no switch turned it off.
	Invalidated bool `json:"-"`
	// NoRoute names the adapter whose route can carry no notice: its sender
	// cannot reach the session, or it has no sender. It is set where the
	// adapter reports it, never read back from Reason, whose words another
	// adapter may share.
	NoRoute string `json:"-"`
}

// ReadPublishState reports the publish state of a binding's route. A
// binding without a route has none, and found is false.
func ReadPublishState(ctx context.Context, tx Tx, bindingID int64, now time.Time) (state PublishState, found bool, err error) {
	var (
		routeState, routeReason, adapter, launchRef, bindingState, profile string
		mode, socketPath, threadID                                         string
		principal, resumedAfter                                            int64
		verified, dnd, quiet, enabled, epochOK, current                    bool
		afterOwnerFence, proven, activated                                 bool
		owner, vendor, hall                                                sql.NullBool
		host                                                               publish.Queue
		home                                                               string
	)
	err = tx.QueryRowContext(ctx, `SELECT r.state, r.reason, r.generation, r.adapter, r.launch_ref, r.principal_id,
			r.launch_verified = 1 OR `+PublishThreadRouted()+`, COALESCE(r.mode, ''), COALESCE(r.socket_path, ''), COALESCE(r.thread_id, ''),
			COALESCE(r.host_pid, 0), COALESCE(r.host_start, 0), COALESCE(r.vendor_home, ''), b.state, b.dnd, b.publish_quiet, b.publish_owner,
			b.publish_resume_suspension, COALESCE(policy.enabled, 0), policy.publish,
			COALESCE(policy.epoch = r.policy_epoch, 0), (SELECT h.enabled FROM hall_publish h WHERE h.id = 1),
			COALESCE(`+PublishLaunchBinds()+`, 0),
			COALESCE(`+PublishLaunchAfterOwnerFence+`, 1),
			COALESCE(l.profile, '{}'),
			EXISTS (SELECT 1 FROM publish_route_proof p WHERE p.binding_id = r.binding_id AND p.generation = r.generation),
			EXISTS (SELECT 1 FROM publish_route_proof p WHERE p.binding_id = r.binding_id AND p.generation = r.generation
				AND p.activated_at_ms IS NOT NULL)
		FROM publish_route r JOIN binding b ON b.id = r.binding_id
		LEFT JOIN adapter_policy policy ON policy.adapter = r.adapter
		LEFT JOIN adapter_launch l ON l.rowid = (SELECT current.rowid FROM adapter_launch current
			WHERE current.principal_id = r.principal_id AND current.ended_hook_at_ms IS NULL
			ORDER BY current.started_hook_at_ms DESC, current.rowid DESC LIMIT 1)
		WHERE r.binding_id = ?`, bindingID).Scan(&routeState, &routeReason, &state.Generation, &adapter, &launchRef,
		&principal, &verified, &mode, &socketPath, &threadID, &host.PID, &host.StartMS, &home, &bindingState, &dnd, &quiet, &owner, &resumedAfter, &enabled, &vendor, &epochOK,
		&hall, &current, &afterOwnerFence, &profile, &proven, &activated)
	if errors.Is(err, sql.ErrNoRows) {
		return PublishState{}, false, nil
	}
	if err != nil {
		return PublishState{}, false, err
	}
	off := func(reason string) (PublishState, bool, error) {
		state.State, state.Reason = "off", reason
		return state, true, nil
	}
	// The owner's and the member's switches come first, so a hard stop is
	// reported as such even when it also ended the route.
	switch {
	case bindingState != "live":
		return off("not a live member")
	case !enabled:
		return off("adapter " + adapter + " disabled by the owner")
	case owner.Valid && !owner.Bool:
		return off("turned off by the owner for this member")
	case !owner.Valid && vendor.Valid && !vendor.Bool:
		return off("turned off by the owner for " + adapter)
	case !owner.Valid && !vendor.Valid && hall.Valid && !hall.Bool:
		return off("turned off by the owner for the hall")
	case quiet:
		return off("turned off by the member")
	case dnd:
		return off("do not disturb")
	case routeState != "active":
		state.Invalidated = routeState == "invalidated"
		return off(routeReason)
	case !afterOwnerFence:
		state.State, state.Reason = "candidate", "launch predates owner departure"
		return state, true, nil
	}
	var build capabilityBuildEvidence
	if verified && current {
		build, err = readCapabilityBuild(ctx, tx, adapter, profile, "publish")
		if err != nil {
			return PublishState{}, false, err
		}
	}
	var reason string
	var notice, at, suspensionID int64
	var until sql.NullInt64
	var unknownCause sql.NullString
	var recoveryHint int
	err = tx.QueryRowContext(ctx, `WITH suspensions AS (
		SELECT s.id, s.reason, s.notice, s.at_ms, s.recovery_hint, `+publishSuspensionExpiry+` AS until_ms FROM publish_suspension s
		WHERE s.binding_id IN (SELECT id FROM binding WHERE principal_id = ?) AND s.launch_ref = ? AND s.id > ?
	)
		SELECT id, reason, notice, at_ms, until_ms, recovery_hint,
			(SELECT a.unknown_cause FROM publish_attempt a WHERE a.id = notice)
		FROM suspensions WHERE until_ms IS NULL OR until_ms > ?
		ORDER BY until_ms IS NULL DESC, until_ms DESC, id DESC LIMIT 1`, principal, launchRef, resumedAfter, now.UnixMilli()).
		Scan(&suspensionID, &reason, &notice, &at, &until, &recoveryHint, &unknownCause)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return PublishState{}, false, err
	}
	if err == nil && !until.Valid {
		state.RecoveryHint = PublishRecoveryHint{SuspensionID: suspensionID, State: recoveryHint}
	}
	if build.Stopped {
		state.State, state.Notice, state.AtMS = "suspended", build.Notice, build.StoppedAtMS
		state.Reason = fmt.Sprintf("version %s stopped after notice %d (%s)", build.Version, build.Notice, build.StopReason)
		state.Lift = fmt.Sprintf("lifts with crosshall owner publish resume %d or tested evidence for this version", bindingID)
		return state, true, nil
	}
	if err == nil {
		state.State, state.Notice, state.AtMS, state.UntilMS = "suspended", notice, at, until.Int64
		state.Reason = fmt.Sprintf("notice %d %s", notice, reason)
		if reason == "unknown" && unknownCause.Valid {
			// The cause doctor counts the day's unknown receipts by.
			state.Reason += " (" + strings.ReplaceAll(unknownCause.String, "_", " ") + ")"
		}
		if reason == "not_submitted" {
			state.Reason = fmt.Sprintf("notice %d not submitted, the third in a row", notice)
		}
		resume := fmt.Sprintf("crosshall owner publish resume %d", bindingID)
		state.Lift = "lifts then, when the launch ends, or with " + resume
		if !until.Valid {
			state.Lift = "the third suspension of this launch: lasts until the launch ends, or " + resume
		}
		return state, true, nil
	}
	var reserved int
	var oldest sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), MIN(submitting_at_ms) FROM publish_attempt
		WHERE principal_id = ? AND (state = 'reserved' OR (submitting_at_ms > ? AND state IN ('submitting', 'submitted', 'uncertain')))`,
		principal, now.Add(-time.Hour).UnixMilli()).Scan(&reserved, &oldest); err != nil {
		return PublishState{}, false, err
	}
	if reserved >= PublishHourlyCap && oldest.Valid {
		state.State, state.UntilMS = "capped", oldest.Int64+time.Hour.Milliseconds()
		return state, true, nil
	}
	// The same open lane the reservation indexes enforce: one per binding
	// and one per queue (PublishLane), which another binding can hold.
	err = tx.QueryRowContext(ctx, `SELECT a.id, COALESCE(a.submitting_at_ms, a.reserved_at_ms) FROM publish_attempt a
		WHERE a.observed_at_ms IS NULL AND a.retired_at_ms IS NULL AND a.state <> 'not_submitted'
			AND (a.binding_id = ?1 OR EXISTS (SELECT 1 FROM publish_route r JOIN publish_route_proof p
				ON p.binding_id = r.binding_id AND p.generation = r.generation
				WHERE r.binding_id = ?1 AND a.lane = `+PublishLane()+`))
		ORDER BY a.id DESC LIMIT 1`, bindingID).Scan(&notice, &at)
	if err == nil {
		state.State, state.Notice, state.AtMS = "held", notice, at
		return state, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return PublishState{}, false, err
	}
	state.State = "candidate"
	adapters := publish.Adapters()
	route := publish.Route{Mode: mode, SocketPath: socketPath, ThreadID: threadID, Host: host, Home: home}
	_, why := adapters.Address(adapter, route)
	var reach string
	if a, ok := adapters.Lookup(adapter); ok {
		reach = a.Reach(route)
	}
	switch {
	// A session the adapter cannot reach stays so with any sender.
	case reach != "":
		state.Reason, state.NoRoute = reach, adapter
	case !adapters.Sends(adapter):
		state.Reason, state.NoRoute = PublishNoSender+adapter, adapter
	case !verified:
		state.Reason = "unverified launch"
	case !current:
		state.Reason = "launch not current"
	case !epochOK:
		state.Reason = "adapter policy changed"
	case why != "":
		state.Reason = why
	case !build.Covered:
		state.Reason = build.candidateReason()
	case !proven:
		state.Reason = "route not proven"
	case !activated:
		state.Reason = PublishNotActivated
	default:
		state.State = "available"
		if build.ExactLevel != "tested" {
			if build.Confirmed == PublishBuildConfirmations {
				state.Reason = fmt.Sprintf("version %s seen", build.Version)
			} else {
				state.Reason = fmt.Sprintf("version %s trusted from %s evidence, %d of %d first notices observed",
					build.Version, build.ManifestRev, build.Confirmed, PublishBuildConfirmations)
			}
		}
	}
	return state, true, nil
}

func (build capabilityBuildEvidence) candidateReason() string {
	if build.Version == "unverified" || build.Mode == "unverified" {
		return "unverified profile"
	}
	if build.ExactLevel != "" && build.ExactLevel != "tested" {
		return fmt.Sprintf("version %s exact evidence is %s", build.Version, build.ExactLevel)
	}
	return fmt.Sprintf("no tested evidence for %s on %s %s", build.ManifestRev, build.Platform, build.Mode)
}

func switchWord(value sql.NullBool) string {
	switch {
	case !value.Valid:
		return ""
	case value.Bool:
		return "on"
	default:
		return "off"
	}
}
