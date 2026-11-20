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

// PublishHourlyCap bounds the notices one member can be offered in an hour.
const PublishHourlyCap = 30

// Publish eligibility shared by the publisher and the status views. Each
// fragment reads the route r, its binding b, the adapter policy policy and
// the member's current launch l.
const (
	// PublishSwitchesOn: the adapter is enabled, the narrowest owner switch
	// set (member, then adapter, then hall) is on or none is set, and the
	// member has not turned publish off for itself.
	PublishSwitchesOn = `policy.enabled = 1 AND b.publish_quiet = 0
		AND COALESCE(b.publish_owner, policy.publish, (SELECT h.enabled FROM hall_publish h WHERE h.id = 1), 1) = 1`
	// PublishProfileTested: the profile list holds the launch's exact
	// profile as tested for publish.
	PublishProfileTested = `EXISTS (SELECT 1 FROM capability_evidence e WHERE e.vendor = r.adapter
		AND e.cap = 'publish' AND e.level = 'tested'
		AND e.vendor_version = json_extract(l.profile, '$.vendor_version')
		AND e.platform = json_extract(l.profile, '$.platform') AND e.mode = json_extract(l.profile, '$.mode')
		AND e.manifest_rev = json_extract(l.profile, '$.manifest_rev'))`
)

// PublishAdapterReady holds when the route's adapter has a notice format
// in this build and the route names an address.
var PublishAdapterReady = publishAdapterReady(publish.Shipped().Publishing())

func publishAdapterReady(names []string) string {
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = "'" + strings.ReplaceAll(name, "'", "''") + "'"
	}
	return "r.adapter IN (" + strings.Join(quoted, ", ") + ") AND COALESCE(r.socket_path, r.thread_id) IS NOT NULL"
}

// PublishNotSuspended holds when no suspension of the route's launch,
// committed after the owner's last resume, is still in force at nowMS.
func PublishNotSuspended(nowMS int64) string {
	return fmt.Sprintf(`NOT EXISTS (SELECT 1 FROM publish_suspension s WHERE s.binding_id = r.binding_id
		AND s.launch_ref = r.launch_ref AND s.id > b.publish_resume_suspension
		AND (s.until_ms IS NULL OR s.until_ms > %d))`, nowMS)
}

// PublishState is one member's publish state as who and doctor show it.
// State is off, candidate, available, held, suspended or capped. Notice is
// the attempt a held or suspended state refers to; AtMS is when it was
// held or suspended, UntilMS when a cap or a timed suspension ends. Lift
// says how a suspension ends. Agents see State only.
type PublishState struct {
	State      string `json:"state"`
	Reason     string `json:"reason,omitempty"`
	Notice     int64  `json:"notice,omitempty"`
	AtMS       int64  `json:"at_ms,omitempty"`
	UntilMS    int64  `json:"until_ms,omitempty"`
	Lift       string `json:"lift,omitempty"`
	Generation int64  `json:"generation,omitempty"`
}

// ReadPublishState reports the publish state of a binding's route. A
// binding without a route has none, and found is false.
func ReadPublishState(ctx context.Context, tx Tx, bindingID int64, now time.Time) (state PublishState, found bool, err error) {
	var (
		routeState, routeReason, adapter, launchRef, bindingState, version string
		socketPath, threadID                                               string
		principal, resumedAfter                                            int64
		verified, dnd, quiet, enabled, epochOK, current                    bool
		proven, tested, listed, versionListed                              bool
		owner, vendor, hall                                                sql.NullBool
	)
	err = tx.QueryRowContext(ctx, `SELECT r.state, r.reason, r.generation, r.adapter, r.launch_ref, r.principal_id,
			r.launch_verified, COALESCE(r.socket_path, ''), COALESCE(r.thread_id, ''), b.state, b.dnd, b.publish_quiet, b.publish_owner,
			b.publish_resume_suspension, COALESCE(policy.enabled, 0), policy.publish,
			COALESCE(policy.epoch = r.policy_epoch, 0), (SELECT h.enabled FROM hall_publish h WHERE h.id = 1),
			COALESCE(l.launch_ref = r.launch_ref AND l.verified = 1, 0),
			COALESCE(json_extract(l.profile, '$.vendor_version'), ''),
			EXISTS (SELECT 1 FROM publish_route_proof p WHERE p.binding_id = r.binding_id AND p.generation = r.generation),
			`+PublishProfileTested+`,
			EXISTS (SELECT 1 FROM capability_evidence e WHERE e.vendor = r.adapter AND e.cap = 'publish'),
			EXISTS (SELECT 1 FROM capability_evidence e WHERE e.vendor = r.adapter AND e.cap = 'publish'
				AND e.vendor_version = json_extract(l.profile, '$.vendor_version'))
		FROM publish_route r JOIN binding b ON b.id = r.binding_id
		LEFT JOIN adapter_policy policy ON policy.adapter = r.adapter
		LEFT JOIN adapter_launch l ON l.rowid = (SELECT current.rowid FROM adapter_launch current
			WHERE current.principal_id = r.principal_id AND current.ended_hook_at_ms IS NULL
			ORDER BY current.started_hook_at_ms DESC, current.rowid DESC LIMIT 1)
		WHERE r.binding_id = ?`, bindingID).Scan(&routeState, &routeReason, &state.Generation, &adapter, &launchRef,
		&principal, &verified, &socketPath, &threadID, &bindingState, &dnd, &quiet, &owner, &resumedAfter, &enabled, &vendor, &epochOK,
		&hall, &current, &version, &proven, &tested, &listed, &versionListed)
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
		return off(routeReason)
	}
	var reason string
	var notice, at int64
	var until sql.NullInt64
	err = tx.QueryRowContext(ctx, `SELECT reason, notice, at_ms, until_ms FROM publish_suspension
		WHERE binding_id = ? AND launch_ref = ? AND id > ? AND (until_ms IS NULL OR until_ms > ?)
		ORDER BY until_ms IS NULL DESC, until_ms DESC LIMIT 1`, bindingID, launchRef, resumedAfter, now.UnixMilli()).
		Scan(&reason, &notice, &at, &until)
	if err == nil {
		state.State, state.Notice, state.AtMS, state.UntilMS = "suspended", notice, at, until.Int64
		state.Reason = fmt.Sprintf("notice %d %s", notice, reason)
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
	if !errors.Is(err, sql.ErrNoRows) {
		return PublishState{}, false, err
	}
	if current && listed && !versionListed {
		state.State, state.Reason, state.Lift = "suspended", fmt.Sprintf("version %q not in the profile list", version),
			"lifts when the launch ends, or when the profile list holds the version (crosshall owner adapter evidence <file>); resume does not lift it"
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
	// and one per physical queue, which another binding can hold.
	err = tx.QueryRowContext(ctx, `SELECT a.id, COALESCE(a.submitting_at_ms, a.reserved_at_ms) FROM publish_attempt a
		WHERE a.observed_at_ms IS NULL AND a.retired_at_ms IS NULL AND a.state <> 'not_submitted'
			AND (a.binding_id = ?1 OR EXISTS (SELECT 1 FROM publish_route r JOIN publish_route_proof p
				ON p.binding_id = r.binding_id AND p.generation = r.generation
				WHERE r.binding_id = ?1 AND p.process_pid = a.process_pid AND p.process_start_ms = a.process_start_ms))
		ORDER BY a.id DESC LIMIT 1`, bindingID).Scan(&notice, &at)
	if err == nil {
		state.State, state.Notice, state.AtMS = "held", notice, at
		return state, true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return PublishState{}, false, err
	}
	state.State = "candidate"
	sender, known := publish.Lookup(adapter)
	notices := false
	if known {
		_, notices = sender.Notice(nil)
	}
	complete, why := known && notices, ""
	if complete {
		complete, why = sender.Complete(publish.Route{SocketPath: socketPath, ThreadID: threadID})
	}
	switch {
	case !known || !notices:
		state.Reason = "no sender for " + adapter
	case !verified:
		state.Reason = "unverified launch"
	case !current:
		state.Reason = "launch not current"
	case !epochOK:
		state.Reason = "adapter policy changed"
	case !complete:
		state.Reason = why
	case !tested:
		state.Reason = "profile not tested"
	case !proven:
		state.Reason = "route not proven"
	default:
		state.State = "available"
	}
	return state, true, nil
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
