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
// replaced by /clear: the start says "clear", and a member's launch in the
// same process, which still runs, came before it. The new key joins that
// principal, so the binding, its open requests and its owed replies stay;
// the old keys are retired. It returns 0 when no member qualifies.
func (s *Service) takeOverCleared(ctx context.Context, tx store.Tx, req Request, cmd HookEnroll, now time.Time) (int64, error) {
	if cmd.Source != "clear" || !cmd.LaunchVerified || cmd.LaunchRef == SessionLaunch || req.Enroll == nil ||
		s.LaunchProcess == nil || s.LaunchAlive == nil {
		return 0, nil
	}
	own, ok := s.LaunchProcess(cmd.LaunchRef)
	if !ok {
		return 0, nil
	}
	var principal, began int64
	var ref string
	err := tx.QueryRowContext(ctx, `SELECT l.principal_id, l.launch_ref, l.started_hook_at_ms
		FROM adapter_launch l JOIN principal p ON p.id = l.principal_id
		JOIN binding b ON b.principal_id = l.principal_id
		WHERE p.kind = 'session' AND p.vendor = ? AND (l.launch_ref = ? OR l.launch_ref LIKE ? || '-%')
			AND l.started_hook_at_ms < ?
			AND (b.state = 'live' OR b.departed_by_hook = 1)
			AND (b.owner_hook_fence_ms IS NULL OR b.owner_hook_fence_ms < ?)
		ORDER BY l.started_hook_at_ms DESC, l.rowid DESC LIMIT 1`,
		req.Vendor, own, own, cmd.HookAtMS, cmd.HookAtMS).Scan(&principal, &ref, &began)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	// The same PID in a process born after that launch is another program.
	if process, ok := s.LaunchProcess(ref); !ok || process != own || !s.LaunchAlive(ref, began) {
		return 0, nil
	}
	// The replaced conversation's keys retire first, so that only the new
	// key is live for the principal once it is enrolled.
	if _, err := tx.ExecContext(ctx, `UPDATE principal_key SET state = 'retired', retired_at_ms = ?
		WHERE principal_id = ? AND state = 'live'`, ms(now), principal); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE leaf SET state = 'fenced' WHERE state = 'live' AND (vendor, key) IN
		(SELECT vendor, key FROM principal_key WHERE principal_id = ? AND state = 'retired')`, principal); err != nil {
		return 0, err
	}
	if err := req.Enroll(ctx, tx, principal); err != nil {
		return 0, err
	}
	return principal, audit(ctx, tx, "session_cleared", "principal:"+strconv.FormatInt(principal, 10), principal,
		map[string]string{"launch": cmd.LaunchRef}, now)
}

// bindJoinLaunch binds the launch a hand join names, so that the session's
// hooks and a later /clear can tell its process: the reference has a start
// digest, its adapter is the session's, its time is the join's, its process
// runs and started before it, and no launch of that process is open for the
// principal (a start already bound one). It returns the bound reference, or "".
func (s *Service) bindJoinLaunch(ctx context.Context, tx store.Tx, principal int64, l JoinLaunch, now time.Time) (string, error) {
	if s.LaunchProcess == nil || s.LaunchAlive == nil || !validProfile(l.Profile) || !validRef(l.Ref) ||
		l.AtMS > ms(now)+futureSlackMS || l.AtMS < ms(now)-joinLaunchAgeMS {
		return "", nil
	}
	own, ok := s.LaunchProcess(l.Ref)
	if !ok || len(l.Ref) <= len(own)+1 || !s.LaunchAlive(l.Ref, l.AtMS) {
		return "", nil
	}
	var free bool
	if err := tx.QueryRowContext(ctx, `SELECT vendor = ?2 AND NOT EXISTS(SELECT 1 FROM adapter_launch WHERE principal_id = ?1
		AND ended_hook_at_ms IS NULL AND (launch_ref = ?3 OR launch_ref LIKE ?3 || '-%'))
		FROM principal WHERE id = ?1`, principal, l.Profile.Adapter, own).Scan(&free); err != nil || !free {
		return "", err
	}
	if err := registerLaunch(ctx, tx, principal, l.Profile, l.Ref, true, l.AtMS); err != nil {
		return "", err
	}
	if _, _, err := s.settleLaunches(ctx, tx, principal, l.Ref, l.AtMS, now); err != nil {
		return "", err
	}
	return l.Ref, refreshMemberAdapterCapabilities(ctx, tx, principal, l.Profile.Adapter)
}

// joinLaunchAgeMS bounds how old a join's launch time may be when the
// dispatcher sees it: one command's budget, with room for a slow start.
const joinLaunchAgeMS = 30_000
