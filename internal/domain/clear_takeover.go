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
	own, ok := s.LaunchProcess(cmd.LaunchRef)
	if !ok {
		return 0, false, nil
	}
	// The process's latest launch decides, whatever its member did since.
	var began int64
	var ref string
	err = tx.QueryRowContext(ctx, `SELECT l.principal_id, l.launch_ref, l.started_hook_at_ms
		FROM adapter_launch l JOIN principal p ON p.id = l.principal_id
		WHERE p.kind = 'session' AND p.vendor = ? AND (l.launch_ref = ? OR l.launch_ref LIKE ? || '-%')
			AND l.started_hook_at_ms < ?
		ORDER BY l.started_hook_at_ms DESC, l.rowid DESC LIMIT 1`,
		req.Vendor, own, own, cmd.HookAtMS).Scan(&principal, &ref, &began)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	// The same PID in a process born after that launch is another program.
	if process, ok := s.LaunchProcess(ref); !ok || process != own || !s.LaunchAlive(ref, began) {
		return 0, false, nil
	}
	var state string
	var byHook, fenced bool
	err = tx.QueryRowContext(ctx, `SELECT state, departed_by_hook, COALESCE(owner_hook_fence_ms >= ?, 0)
		FROM binding WHERE principal_id = ? ORDER BY id LIMIT 1`, cmd.HookAtMS, principal).Scan(&state, &byHook, &fenced)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if fenced || (state != "live" && !byHook) {
		return 0, true, nil
	}
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
// runs and started before it, and no member of that adapter has a launch
// open on the process (a start already bound one). It returns the bound
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
	// One process runs one conversation: no member of the adapter may have a
	// launch open on it.
	var free bool
	if err := tx.QueryRowContext(ctx, `SELECT vendor = ?2 AND NOT EXISTS(SELECT 1 FROM adapter_launch l
		JOIN principal o ON o.id = l.principal_id AND o.vendor = ?2
		WHERE l.ended_hook_at_ms IS NULL AND (l.launch_ref = ?3 OR l.launch_ref LIKE ?3 || '-%'))
		FROM principal WHERE id = ?1`, principal, l.Profile.Adapter, own).Scan(&free); err != nil || !free {
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
