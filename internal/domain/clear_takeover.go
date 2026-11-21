package domain

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

// takeOverCleared gives a new session key the member of the conversation it
// replaced by /clear: the start says "clear", its hook runs under the process
// it names, and that process, which still runs, last hosted a member's
// launch. The new key joins that principal, so the binding, its open
// requests and its owed replies stay; the old keys retire as cleared. It
// returns the principal, or 0 with quiet set when the process's member left
// by hand or was departed by the owner: such a clear joins nobody.
func (s *Service) takeOverCleared(ctx context.Context, tx store.Tx, req Request, cmd HookEnroll, now time.Time) (principal int64, quiet bool, err error) {
	if cmd.Source != "clear" || !cmd.LaunchVerified || !cmd.LaunchHosted || cmd.LaunchRef == SessionLaunch ||
		req.Enroll == nil || s.LaunchProcess == nil || s.LaunchAlive == nil {
		return 0, false, nil
	}
	launch, found, err := s.processLaunchAt(ctx, tx, req.Vendor, cmd.LaunchRef, cmd.HookAtMS)
	if err != nil || !found {
		return 0, false, err
	}
	if launch.away {
		return 0, true, nil
	}
	principal = launch.principal
	// The replaced conversation's keys retire first, so that only the new
	// key is live for the principal once it is enrolled.
	if _, err := tx.ExecContext(ctx, `UPDATE principal_key SET state = 'retired', retired_at_ms = ?, retired_by = 'clear'
		WHERE principal_id = ? AND state = 'live'`, ms(now), principal); err != nil {
		return 0, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE leaf SET state = 'fenced' WHERE state = 'live' AND (vendor, key) IN
		(SELECT vendor, key FROM principal_key WHERE principal_id = ? AND state = 'retired')`, principal); err != nil {
		return 0, false, err
	}
	if err := req.Enroll(ctx, tx, principal); err != nil {
		return 0, false, err
	}
	return principal, false, audit(ctx, tx, "session_cleared", "principal:"+strconv.FormatInt(principal, 10), principal,
		map[string]string{"launch": cmd.LaunchRef}, now)
}

// bindJoinLaunch binds the launch a hand join names, so that the session's
// hooks and a later /clear can tell its process: the reference has a start
// digest, its adapter is the session's, its time is the join's, its process
// runs and started before it, and no present member of that adapter has a
// launch open on the process (a start already bound one). It returns the bound
// launch, or nil.
func (s *Service) bindJoinLaunch(ctx context.Context, tx store.Tx, principal int64, l JoinLaunch, now time.Time) (*JoinLaunch, error) {
	if s.LaunchProcess == nil || s.LaunchAlive == nil || !validProfile(l.Profile) || !validRef(l.Ref) ||
		l.AtMS > ms(now)+futureSlackMS || l.AtMS < ms(now)-joinLaunchAgeMS {
		return nil, nil
	}
	own, ok := s.LaunchProcess(l.Ref)
	if !ok || len(l.Ref) <= len(own)+1 || !s.LaunchAlive(l.Ref, l.AtMS) {
		return nil, nil
	}
	var vendor string
	if err := tx.QueryRowContext(ctx, `SELECT vendor FROM principal WHERE id = ?`, principal).Scan(&vendor); err != nil || vendor != l.Profile.Adapter {
		return nil, err
	}
	// One process runs one conversation: no present member of the adapter
	// may have a launch open on it that still runs (an unended launch of a
	// process gone without an end does not count; the PID may be reused).
	// A member that left by hand or was departed by the owner does not
	// count either: this join is the explicit one that ends its absence.
	rows, err := tx.QueryContext(ctx, `SELECT l.launch_ref, l.started_hook_at_ms FROM adapter_launch l
		JOIN principal o ON o.id = l.principal_id AND o.vendor = ?1
		JOIN binding b ON b.id = (SELECT MIN(id) FROM binding WHERE principal_id = l.principal_id)
		WHERE l.ended_hook_at_ms IS NULL AND l.fenced = 0 AND (l.launch_ref = ?2 OR l.launch_ref LIKE ?2 || '-%')
			AND (b.state = 'live' OR b.departed_by_hook = 1) AND COALESCE(b.owner_hook_fence_ms < ?3, 1)`,
		vendor, own, l.AtMS)
	if err != nil {
		return nil, err
	}
	occupied := false
	for rows.Next() {
		var ref string
		var began int64
		if err := rows.Scan(&ref, &began); err != nil {
			rows.Close()
			return nil, err
		}
		occupied = occupied || s.LaunchAlive(ref, began)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil || occupied {
		return nil, err
	}
	if err := registerLaunch(ctx, tx, principal, l.Profile, l.Ref, true, l.AtMS); err != nil {
		return nil, err
	}
	if _, _, err := s.settleLaunches(ctx, tx, principal, l.Ref, l.AtMS, now); err != nil {
		return nil, err
	}
	return &l, refreshMemberAdapterCapabilities(ctx, tx, principal, l.Profile.Adapter)
}

// joinLaunchAgeMS bounds how old a join's launch time may be when the
// dispatcher sees it: one command's budget, with room for a slow start.
const joinLaunchAgeMS = 30_000

// processLaunch is the latest launch a process ran for a member.
type processLaunch struct {
	principal int64
	// away says its member left by hand or was departed by the owner, so
	// nothing in that process joins until an explicit join.
	away bool
}

// processLaunchAt finds the latest launch the process of launchRef ran
// before at, among members of vendor, if that process still runs (a reused
// PID is a process born after the launch). A launch an owner restore fenced
// belongs to no one.
func (s *Service) processLaunchAt(ctx context.Context, tx store.Tx, vendor, launchRef string, at int64) (processLaunch, bool, error) {
	var none processLaunch
	if s.LaunchProcess == nil || s.LaunchAlive == nil {
		return none, false, nil
	}
	own, ok := s.LaunchProcess(launchRef)
	if !ok {
		return none, false, nil
	}
	var l processLaunch
	var ref, state string
	var began int64
	var byHook, fenced bool
	err := tx.QueryRowContext(ctx, `SELECT l.principal_id, l.launch_ref, l.started_hook_at_ms,
			b.state, b.departed_by_hook, COALESCE(b.owner_hook_fence_ms >= ?4, 0)
		FROM adapter_launch l JOIN principal p ON p.id = l.principal_id
		JOIN binding b ON b.id = (SELECT MIN(id) FROM binding WHERE principal_id = l.principal_id)
		WHERE p.kind = 'session' AND p.vendor = ?1 AND l.fenced = 0
			AND (l.launch_ref = ?2 OR l.launch_ref LIKE ?3 || '-%') AND l.started_hook_at_ms < ?4
		ORDER BY l.started_hook_at_ms DESC, l.rowid DESC LIMIT 1`,
		vendor, own, own, at).Scan(&l.principal, &ref, &began, &state, &byHook, &fenced)
	if errors.Is(err, sql.ErrNoRows) {
		return none, false, nil
	}
	if err != nil {
		return none, false, err
	}
	if process, ok := s.LaunchProcess(ref); !ok || process != own || !s.LaunchAlive(ref, began) {
		return none, false, nil
	}
	l.away = fenced || (state != "live" && !byHook)
	return l, true, nil
}
