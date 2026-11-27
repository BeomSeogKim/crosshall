package domain

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// lateLaunchOf is the launch a prompt names with the time its hook recorded
// it (#252): a verified process launch, recorded no later than the prompt,
// which is not ahead of the dispatcher's clock.
func lateLaunchOf(cmd HookEvent, now time.Time) (JoinLaunch, bool) {
	if cmd.Fact != FactPromptSubmitted || !cmd.LaunchVerified || cmd.LaunchRef == SessionLaunch ||
		cmd.LaunchAtMS <= 0 || cmd.LaunchAtMS > cmd.HookAtMS || cmd.HookAtMS > ms(now)+futureSlackMS {
		return JoinLaunch{}, false
	}
	return JoinLaunch{Ref: cmd.LaunchRef, AtMS: cmd.LaunchAtMS, Profile: cmd.Profile, PIDNamespace: cmd.PIDNamespace}, true
}

// bindLateLaunch binds the launch of a prompt whose start the dispatcher
// never saw, under the checks of a hand join, and registers the route that
// start would have registered. It reports whether it bound the launch; a
// refusal writes nothing.
func (s *Service) bindLateLaunch(ctx context.Context, tx store.Tx, req Request, principal int64, b hookBinding,
	cmd HookEvent, now time.Time) (bool, error) {
	l, ok := lateLaunchOf(cmd, now)
	if !ok {
		return false, nil
	}
	if known, err := launchKnown(ctx, tx, principal, l.Ref); err != nil || known {
		return false, err
	}
	bound, err := s.bindJoinLaunch(ctx, tx, principal, l, now)
	if err != nil || bound == nil {
		return false, err
	}
	// As at a start: the recorded host may be the one this launch replaced.
	if err := forgetHost(ctx, tx, principal); err != nil {
		return false, err
	}
	// The route is registered at the prompt's time, not the launch's: the
	// prompt saw the binding, which may have been joined after the launch.
	enroll := HookEnroll{wire.HookEnrollArgs{Profile: l.Profile, LaunchRef: l.Ref, LaunchVerified: true,
		PIDNamespace: l.PIDNamespace, EventRef: cmd.EventRef, Source: "late", HookAtMS: cmd.HookAtMS, Epoch: cmd.Epoch}}
	if err := registerPublishRouteFromEnroll(ctx, tx, b.id, b.routeEpoch, b.joinedAtMS, principal, req.SessionKey, enroll,
		s.routeHost(enroll), now); err != nil {
		return false, err
	}
	return true, audit(ctx, tx, "launch_bound_late", "binding:"+strconv.FormatInt(b.id, 10), principal,
		map[string]string{"launch": l.Ref}, now)
}

// bindMemberLateLaunch is the late bind of a member the hall knows, where its
// start would have restored it: not left by hand, not behind the owner's
// fence, and recorded after its last clean end. A member that ended by its
// hooks rejoins.
func (s *Service) bindMemberLateLaunch(ctx context.Context, tx store.Tx, req Request, principal int64, cmd HookEvent,
	now time.Time) (bool, error) {
	l, ok := lateLaunchOf(cmd, now)
	if !ok {
		return false, nil
	}
	// Every prompt of a process launch carries its time: the common case
	// is a launch whose start was seen.
	if known, err := launchKnown(ctx, tx, principal, l.Ref); err != nil || known {
		return false, err
	}
	b, found, err := bindingOf(ctx, tx, principal)
	if err != nil || !found || !b.acceptsHookAt(cmd.HookAtMS) ||
		b.ownerHookFence.Valid && (b.state != "live" || l.AtMS <= b.ownerHookFence.Int64) {
		return false, err
	}
	if b.state != "live" {
		var lastEnd sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MAX(CASE WHEN ended_by_process = 1 THEN started_hook_at_ms ELSE ended_hook_at_ms END)
			FROM adapter_launch WHERE principal_id = ?`, principal).Scan(&lastEnd); err != nil {
			return false, err
		}
		if !b.byHook || lastEnd.Valid && l.AtMS <= lastEnd.Int64 {
			return false, nil
		}
	}
	before, err := presenceOf(ctx, tx, principal, now)
	if err != nil {
		return false, err
	}
	bound, err := s.bindLateLaunch(ctx, tx, req, principal, b, cmd, now)
	if err != nil || !bound {
		return false, err
	}
	if b.state != "live" {
		if _, err := rejoin(ctx, tx, b, principal, req.Vendor, now); err != nil {
			return false, err
		}
		// The capabilities follow the live binding.
		if err := refreshMemberAdapterCapabilities(ctx, tx, principal, l.Profile.Adapter); err != nil {
			return false, err
		}
	}
	return true, presenceChanged(ctx, tx, b.channel, principal, before, now)
}

func launchKnown(ctx context.Context, tx store.Tx, principal int64, launch string) (bool, error) {
	var known bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM adapter_launch WHERE principal_id = ? AND launch_ref = ?)`,
		principal, launch).Scan(&known)
	return known, err
}

// startUnseen reports whether the member's current launch has no seen
// start: a prompt joined it and its launch could not be bound.
// linkedStartUnseen says a live member on a linked device, whose adapter
// can send a notice, has no launch the home could wake in its current
// scope: it joined, moved or rejoined after its session started, and its
// device has sent no prompt since (#437). Its next prompt opens the launch
// in that scope, as a local member's does.
func linkedStartUnseen(ctx context.Context, tx store.Tx, principal, binding int64) (bool, error) {
	var vendor string
	err := tx.QueryRowContext(ctx, `SELECT k.vendor FROM device_key k
		JOIN member_selection s ON s.principal_id = k.principal_id AND s.binding_id = ?2
		JOIN binding b ON b.id = s.binding_id
		WHERE k.principal_id = ?1 AND k.state = 'live'
			AND NOT EXISTS (SELECT 1 FROM link_event_launch l WHERE l.principal_id = k.principal_id
				AND l.device_id = k.device_id AND l.credential_gen = k.credential_gen AND l.leaf_generation = k.leaf_generation
				AND l.binding_id = b.id AND l.membership_generation = s.generation AND l.route_epoch = b.route_epoch)
		LIMIT 1`, principal, binding).Scan(&vendor)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	adapter, ok := publish.Lookup(vendor)
	return ok && adapter.Transport().Send != nil, nil
}

func startUnseen(ctx context.Context, tx store.Tx, principal int64) (bool, error) {
	var unseen bool
	err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT ended_hook_at_ms IS NULL AND started_hook_at_ms = 0
		FROM adapter_launch WHERE principal_id = ?
		ORDER BY ended_hook_at_ms IS NOT NULL, started_hook_at_ms DESC, rowid DESC LIMIT 1), 0)`, principal).Scan(&unseen)
	return unseen, err
}
