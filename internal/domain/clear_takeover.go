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
// by hand or was departed by the owner: such a clear joins nobody. It also
// returns the receipts the replaced conversation recorded and has not sent,
// for the caller to apply once the binding is live again.
func (s *Service) takeOverCleared(ctx context.Context, tx store.Tx, req Request, cmd HookEnroll, now time.Time) (principal int64, quiet bool, receipts clearedReceipts, err error) {
	if cmd.Source != "clear" || !cmd.LaunchVerified || !cmd.LaunchHosted || cmd.LaunchRef == SessionLaunch ||
		req.Enroll == nil || s.LaunchProcess == nil || s.LaunchAlive == nil {
		return 0, false, receipts, nil
	}
	launch, found, err := s.processLaunchAt(ctx, tx, cmd.Profile.Adapter, cmd.LaunchRef, cmd.HookAtMS)
	if err != nil || !found {
		return 0, false, receipts, err
	}
	if launch.away || launch.stale {
		return 0, true, receipts, nil
	}
	principal = launch.principal
	if receipts, err = s.readClearedReceipts(ctx, tx, principal); err != nil {
		return 0, false, receipts, err
	}
	// The replaced conversation's keys retire first, so that only the new
	// key is live for the principal once it is enrolled.
	if _, err := tx.ExecContext(ctx, `UPDATE principal_key SET state = 'retired', retired_at_ms = ?, retired_by = 'clear'
		WHERE principal_id = ? AND state = 'live'`, ms(now), principal); err != nil {
		return 0, false, receipts, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE leaf SET state = 'fenced' WHERE state = 'live' AND (vendor, key) IN
		(SELECT vendor, key FROM principal_key WHERE principal_id = ? AND state = 'retired')`, principal); err != nil {
		return 0, false, receipts, err
	}
	if err := req.Enroll(ctx, tx, principal); err != nil {
		return 0, false, receipts, err
	}
	return principal, false, receipts, audit(ctx, tx, "session_cleared", "principal:"+strconv.FormatInt(principal, 10), principal,
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
		JOIN binding b ON b.id = (SELECT MIN(id) FROM binding WHERE principal_id = l.principal_id)
		WHERE json_extract(l.profile, '$.adapter') = ?1 AND l.ended_hook_at_ms IS NULL AND l.fenced = 0
			AND (l.launch_ref = ?2 OR l.launch_ref LIKE ?2 || '-%')
			AND (b.state = 'live' OR b.departed_by_hook = 1) AND COALESCE(b.owner_hook_fence_ms < ?3, 1)`,
		l.Profile.Adapter, own, l.AtMS)
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
	if err := registerLaunch(ctx, tx, principal, l.Profile, l.Ref, true, "", l.AtMS); err != nil {
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
	// stale says the member started again at or after the event: a delayed
	// older clear changes nothing.
	stale bool
}

// processLaunchAt finds the latest launch the process of launchRef ran
// before at for adapter (as the launch recorded it, whatever a restore did
// to its principal), if that process still runs (a reused
// PID is a process born after the launch). When that launch was fenced by an
// owner restore, the process belongs to no one.
func (s *Service) processLaunchAt(ctx context.Context, tx store.Tx, adapter, launchRef string, at int64) (processLaunch, bool, error) {
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
	var byHook, fenced, restored bool
	err := tx.QueryRowContext(ctx, `SELECT l.principal_id, l.launch_ref, l.started_hook_at_ms, l.fenced,
			b.state, b.departed_by_hook, COALESCE(b.owner_hook_fence_ms >= ?4, 0)
		FROM adapter_launch l JOIN principal p ON p.id = l.principal_id
		JOIN binding b ON b.id = (SELECT MIN(id) FROM binding WHERE principal_id = l.principal_id)
		WHERE p.kind = 'session' AND json_extract(l.profile, '$.adapter') = ?1
			AND (l.launch_ref = ?2 OR l.launch_ref LIKE ?3 || '-%') AND l.started_hook_at_ms < ?4
		ORDER BY l.started_hook_at_ms DESC, l.rowid DESC LIMIT 1`,
		adapter, own, own, at).Scan(&l.principal, &ref, &began, &restored, &state, &byHook, &fenced)
	if errors.Is(err, sql.ErrNoRows) {
		return none, false, nil
	}
	if err != nil {
		return none, false, err
	}
	// A launch an owner restore fenced ended that process's ownership.
	if restored {
		return none, false, nil
	}
	if process, ok := s.LaunchProcess(ref); !ok || process != own || !s.LaunchAlive(ref, began) {
		return none, false, nil
	}
	// A later start of the member already happened: this event is older
	// than its latest accepted transition.
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM adapter_launch WHERE principal_id = ? AND started_hook_at_ms >= ?)`,
		l.principal, at).Scan(&l.stale); err != nil {
		return none, false, err
	}
	l.away = fenced || (state != "live" && !byHook)
	return l, true, nil
}

// clearedReceipts are the roster receipts the replaced conversation's
// directories recorded and have not sent yet: the member stays, so the rules
// it was given and the roster it saw stay given and seen. A /clear right
// after a command would otherwise repeat them.
type clearedReceipts struct{ cursor, rules uint64 }

// readClearedReceipts reads them while the replaced keys are still live.
func (s *Service) readClearedReceipts(ctx context.Context, tx store.Tx, principal int64) (clearedReceipts, error) {
	var r clearedReceipts
	if s.LeafReceipts == nil {
		return r, nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT k.vendor, k.key, l.file_identity, l.generation
		FROM principal_key k JOIN leaf l ON l.vendor = k.vendor AND l.key = k.key AND l.state = 'live'
		WHERE k.principal_id = ? AND k.state = 'live'`, principal)
	if err != nil {
		return r, err
	}
	type key struct{ vendor, key, fileID, generation string }
	var keys []key
	for rows.Next() {
		var k key
		if err := rows.Scan(&k.vendor, &k.key, &k.fileID, &k.generation); err != nil {
			rows.Close()
			return r, err
		}
		keys = append(keys, k)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return r, err
	}
	for _, k := range keys {
		cursor, rules := s.LeafReceipts(k.vendor, k.key, k.fileID, k.generation)
		r.cursor, r.rules = max(r.cursor, cursor), max(r.rules, rules)
	}
	return r, nil
}

// apply records them on the member's binding, which must be live: after a
// SessionEnd of the clear, only once the start restored it.
func (r clearedReceipts) apply(ctx context.Context, tx store.Tx, binding int64) error {
	_, _, err := AcknowledgeRoster(ctx, tx, &binding, r.cursor, r.rules)
	return err
}
