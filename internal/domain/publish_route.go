package domain

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

const VerbHookRoute = wire.VerbHookRoute

// HookRoute registers or updates a member's publish route from a hook.
type HookRoute struct{ wire.HookRouteArgs }

func (HookRoute) command()     {}
func (HookRoute) Verb() string { return VerbHookRoute }

// PublishStatus is a member's publish state in who output.
type PublishStatus = store.PublishState

// modeEligible keeps an adapter without its own rule eligible.
func modeEligible(adapter, mode string) bool {
	a, ok := publish.Lookup(adapter)
	return !ok || a.ModeEligible(mode)
}

func routeCandidateReason(adapter string, launchVerified bool, mode, socketPath, threadID string) (string, string) {
	a, ok := publish.Lookup(adapter)
	if !ok {
		return "invalidated", "unsupported adapter"
	}
	if _, why := a.Resolve(publish.Route{Mode: mode, SocketPath: socketPath, ThreadID: threadID}); why != "" {
		return "invalidated", why
	}
	if !launchVerified {
		return "active", "unverified launch"
	}
	return "active", "candidate profile (no sender in slice 1)"
}

func currentWatermark(ctx context.Context, tx store.Tx) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM message`).Scan(&id)
	return id, err
}

func resolveSessionKey(ctx context.Context, tx store.Tx, sessionKey string, principal int64, vendor string) string {
	if sessionKey != "" {
		return sessionKey
	}
	var key string
	_ = tx.QueryRowContext(ctx, `SELECT key FROM principal_key WHERE principal_id = ? AND vendor = ? AND state = 'live'
		ORDER BY rowid DESC LIMIT 1`, principal, vendor).Scan(&key)
	return key
}

func registerPublishRouteFromEnroll(ctx context.Context, tx store.Tx, bindingID, bindingEpoch, joinedAtMS, principal int64, sessionKey string,
	cmd HookEnroll, host publish.Queue, now time.Time) error {
	if bindingEpoch > 1 && cmd.HookAtMS < joinedAtMS {
		return nil
	}
	adapter := cmd.Profile.Adapter
	a, ok := publish.Lookup(adapter)
	if !ok {
		return nil
	}
	sessionRef := resolveSessionKey(ctx, tx, sessionKey, principal, adapter)
	if sessionRef == "" {
		return nil
	}
	route := a.SessionRoute(sessionRef, cmd.Profile.Mode, host)
	return upsertPublishRoute(ctx, tx, bindingID, bindingEpoch, principal, adapter, sessionRef, cmd.LaunchRef, cmd.LaunchVerified,
		cmd.Epoch, route, cmd.HookAtMS, now)
}

// routeHost is the server a session's start named as its host, when the
// dispatcher can judge that process later; else none.
func (s *Service) routeHost(cmd HookEnroll) publish.Queue {
	if !s.judgeableHost(cmd.Host) {
		return publish.Queue{}
	}
	return publish.Queue{PID: cmd.Host.PID, StartMS: cmd.Host.Start}
}

func upsertPublishRoute(ctx context.Context, tx store.Tx, bindingID, bindingEpoch, principal int64, adapter, sessionRef, launchRef string,
	launchVerified bool, epoch uint64, route publish.Route, hookAtMS int64, now time.Time) error {
	mode, socketPath, threadID := route.Mode, route.SocketPath, route.ThreadID
	state, reason := routeCandidateReason(adapter, launchVerified, mode, socketPath, threadID)
	watermark, err := currentWatermark(ctx, tx)
	if err != nil {
		return err
	}
	nowMS := ms(now)
	var modeAtMS int64
	if mode != "" {
		modeAtMS = hookAtMS
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO publish_route (
		binding_id, principal_id, binding_epoch, generation, adapter, session_ref, launch_ref,
		launch_verified, policy_epoch, mode, mode_at_ms, socket_path, thread_id, host_pid, host_start,
		watermark_message_id, state, reason, registered_at_ms, updated_at_ms
	) VALUES (?, ?, ?, 1, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, 0), NULLIF(?, 0), ?, ?, ?, ?, ?)
	ON CONFLICT(binding_id) DO UPDATE SET
		principal_id = excluded.principal_id,
		binding_epoch = excluded.binding_epoch,
		generation = publish_route.generation + 1,
		adapter = excluded.adapter,
		session_ref = excluded.session_ref,
		launch_ref = excluded.launch_ref,
		launch_verified = excluded.launch_verified,
		policy_epoch = excluded.policy_epoch,
		mode = excluded.mode,
		mode_at_ms = excluded.mode_at_ms,
		socket_path = excluded.socket_path,
		thread_id = excluded.thread_id,
		host_pid = excluded.host_pid,
		host_start = excluded.host_start,
		watermark_message_id = excluded.watermark_message_id,
		state = excluded.state,
		reason = excluded.reason,
		registered_at_ms = excluded.registered_at_ms,
		updated_at_ms = excluded.updated_at_ms,
		start_socket = NULL,
		socket_attach = NULL,
		socket_attach_at_ms = NULL
	WHERE excluded.binding_epoch > publish_route.binding_epoch
	   OR (excluded.binding_epoch = publish_route.binding_epoch AND excluded.registered_at_ms >= publish_route.registered_at_ms)`,
		bindingID, principal, bindingEpoch, adapter, sessionRef, launchRef,
		launchVerified, epoch, mode, modeAtMS, socketPath, threadID, route.Host.PID, route.Host.StartMS,
		watermark, state, reason, hookAtMS, nowMS)
	return err
}

// noSocketAtStart is the reason a route without a socket gives when its
// session's start did not see one: a turn's callback can still attach it.
const noSocketAtStart = "no socket at start; the next prompt attaches it"

// attachPublishRouteSocket applies a hook's report of the vendor's socket to
// the member's route for the same live launch, and says whether it updated
// the route as a whole (else the report's mode still applies). A route
// without a socket takes any report's path; the same path again changes
// nothing, so the route keeps its generation and proof; another path
// replaces it only from a session start (#180).
func attachPublishRouteSocket(ctx context.Context, tx store.Tx, bindingID, bindingEpoch, principal int64, cmd HookRoute, sessionRef string, now time.Time) (bool, error) {
	var (
		curAdapter, curSession, curLaunch, curMode, curState string
		curBindingEpoch                                      int64
		curSocket, curThread                                 sql.NullString
		policyEpoch                                          uint64
		modeAtMS, registeredAtMS                             int64
	)
	err := tx.QueryRowContext(ctx, `SELECT binding_epoch, adapter, session_ref, launch_ref, policy_epoch,
		mode, mode_at_ms, socket_path, thread_id, state, registered_at_ms
		FROM publish_route WHERE binding_id = ?`, bindingID).
		Scan(&curBindingEpoch, &curAdapter, &curSession, &curLaunch, &policyEpoch,
			&curMode, &modeAtMS, &curSocket, &curThread, &curState, &registeredAtMS)
	if errors.Is(err, sql.ErrNoRows) {
		if cmd.SocketPath == "" {
			return false, nil
		}
		if err := upsertPublishRoute(ctx, tx, bindingID, bindingEpoch, principal, cmd.Adapter, sessionRef, cmd.LaunchRef, cmd.LaunchVerified,
			cmd.Epoch, publish.Route{Mode: cmd.PermissionMode, SocketPath: cmd.SocketPath}, cmd.HookAtMS, now); err != nil {
			return false, err
		}
		return true, recordSocketAttach(ctx, tx, bindingID, cmd, attachOutcome(cmd), now)
	}
	if err != nil {
		return false, err
	}
	if curBindingEpoch != bindingEpoch || curAdapter != cmd.Adapter || curSession != sessionRef ||
		curLaunch != cmd.LaunchRef || policyEpoch != cmd.Epoch || cmd.HookAtMS < registeredAtMS || curState == "ended" {
		return true, nil
	}
	if cmd.Start {
		seen := "missing"
		if cmd.SocketPath != "" {
			seen = "seen"
		}
		if _, err := tx.ExecContext(ctx, `UPDATE publish_route SET start_socket = ? WHERE binding_id = ?`, seen, bindingID); err != nil {
			return false, err
		}
	}
	switch {
	case cmd.SocketPath == "":
		return false, nil
	case curSocket.String == cmd.SocketPath:
		return false, recordSocketAttach(ctx, tx, bindingID, cmd, "same", now)
	case curSocket.String != "" && !cmd.Start:
		return false, recordSocketAttach(ctx, tx, bindingID, cmd, "ignored", now)
	}
	effectiveMode, effectiveModeAtMS := curMode, modeAtMS
	if cmd.PermissionMode != "" && cmd.HookAtMS > modeAtMS {
		effectiveMode, effectiveModeAtMS = cmd.PermissionMode, cmd.HookAtMS
	}
	newState, newReason := routeCandidateReason(curAdapter, cmd.LaunchVerified, effectiveMode, cmd.SocketPath, curThread.String)
	nowMS := ms(now)
	if newState != "active" {
		_, err = tx.ExecContext(ctx, `UPDATE publish_route
			SET socket_path = ?, mode = ?, mode_at_ms = ?, state = ?, reason = ?, generation = generation + 1, updated_at_ms = ?
			WHERE binding_id = ?`,
			cmd.SocketPath, effectiveMode, effectiveModeAtMS, newState, newReason, nowMS, bindingID)
		if err != nil {
			return false, err
		}
		return true, recordSocketAttach(ctx, tx, bindingID, cmd, attachOutcome(cmd), now)
	}
	watermark, err := currentWatermark(ctx, tx)
	if err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE publish_route
		SET socket_path = ?, mode = ?, mode_at_ms = ?, state = 'active', reason = ?,
		    watermark_message_id = CASE WHEN state <> 'active' THEN ? ELSE watermark_message_id END,
		    generation = generation + 1, updated_at_ms = ?
		WHERE binding_id = ?`,
		cmd.SocketPath, effectiveMode, effectiveModeAtMS, newReason, watermark, nowMS, bindingID)
	if err != nil {
		return false, err
	}
	return true, recordSocketAttach(ctx, tx, bindingID, cmd, attachOutcome(cmd), now)
}

func attachOutcome(cmd HookRoute) string {
	if cmd.Start {
		return "start"
	}
	return "later"
}

func recordSocketAttach(ctx context.Context, tx store.Tx, bindingID int64, cmd HookRoute, outcome string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE publish_route SET socket_attach = ?, socket_attach_at_ms = ? WHERE binding_id = ?`,
		outcome, ms(now), bindingID)
	return err
}

func (s *Service) hookRoute(ctx context.Context, tx store.Tx, caller core.Caller, req Request, cmd HookRoute, now time.Time) (core.Result, error) {
	if caller.Role != core.RoleMember || caller.PrincipalID == nil {
		return Refuse(core.CodeOwnerOnly, "hook routes are for live member sessions", "run crosshall join <channel>"), nil
	}
	if !ValidAdapterName(cmd.Adapter) || cmd.LaunchRef == "" || !validRef(cmd.LaunchRef) ||
		cmd.EventRef == "" || !validRef(cmd.EventRef) || !validRef(cmd.PermissionMode) || cmd.HookAtMS <= 0 {
		return hookUsage("invalid hook.route arguments"), nil
	}
	if a, ok := publish.Lookup(cmd.Adapter); cmd.SocketPath != "" && (!ok || !a.AcceptsSocket(cmd.SocketPath)) {
		if cmd.PermissionMode == "" {
			return Refuse("", "socket path is outside the vendor's socket directory", "use the session's vendor socket path"), nil
		}
		// The path is never stored, but the report's mode still applies: a
		// switch into a mode that stops publishing must not be lost with it.
		cmd.SocketPath, cmd.Start = "", false
	}
	out := wire.HookResult{Epoch: cmd.Epoch}
	if open, err := policyOpen(ctx, tx, cmd.Adapter, cmd.Epoch); err != nil || !open {
		return okOrErr(out, err)
	}
	principal := *caller.PrincipalID
	b, found, err := bindingOf(ctx, tx, principal)
	if err != nil {
		return core.Result{}, err
	}
	if !found || b.state != "live" {
		return Refuse(core.CodeNotEnrolled, "hook route requires a live member", "crosshall join <channel>"), nil
	}
	if caller.BindingID != nil && *caller.BindingID != b.id {
		return Refuse(core.CodeLeafMismatch, "binding does not match authenticated leaf", "re-enroll from the active session"), nil
	}
	if b.ownerHookFence.Valid && cmd.HookAtMS <= b.ownerHookFence.Int64 {
		return OK(out)
	}
	if b.routeEpoch > 1 && cmd.HookAtMS < b.joinedAtMS {
		return Refuse(core.CodeLeafMismatch, "stale pre-rejoin route registration", "re-enroll from the active session"), nil
	}
	if cmd.HookAtMS > ms(now)+futureSlackMS {
		return OK(out)
	}
	latest, err := latestLaunch(ctx, tx, principal)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return core.Result{}, err
	}
	if err != nil || latest != cmd.LaunchRef {
		return OK(out)
	}
	sessionRef := resolveSessionKey(ctx, tx, req.SessionKey, principal, cmd.Adapter)
	if sessionRef == "" {
		return Refuse(core.CodeLeafMismatch, "missing authenticated session key", "re-enroll from the active session"), nil
	}
	if claimed, err := claimHook(ctx, tx, principal, req.OpKey, cmd, now); err != nil || !claimed {
		return okOrErr(out, err)
	}
	if cmd.SocketPath != "" || cmd.Start {
		done, err := attachPublishRouteSocket(ctx, tx, b.id, b.routeEpoch, principal, cmd, sessionRef, now)
		if err != nil {
			return core.Result{}, err
		}
		if done {
			return OK(out)
		}
	}
	if cmd.PermissionMode != "" {
		if err := updatePublishRouteMode(ctx, tx, b.id, b.routeEpoch, cmd.Adapter, sessionRef, cmd.LaunchRef, cmd.Epoch, cmd.PermissionMode, cmd.HookAtMS, now); err != nil {
			return core.Result{}, err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE publish_route SET reason = ?
		WHERE binding_id = ? AND state = 'invalidated' AND reason = 'no socket' AND start_socket = 'missing'`, noSocketAtStart, b.id)
	return okOrErr(out, err)
}

func updatePublishRouteMode(ctx context.Context, tx store.Tx, bindingID, bindingEpoch int64, adapter, sessionRef, launchRef string,
	epoch uint64, newMode string, hookAtMS int64, now time.Time) error {
	var (
		curAdapter, curSession, curLaunch, curMode, curState string
		curBindingEpoch                                      int64
		curSocket, curThread                                 sql.NullString
		launchVerified                                       bool
		policyEpoch                                          uint64
		modeAtMS, registeredAtMS                             int64
	)
	err := tx.QueryRowContext(ctx, `SELECT binding_epoch, adapter, session_ref, launch_ref, launch_verified, policy_epoch,
		mode, mode_at_ms, socket_path, thread_id, state, registered_at_ms
		FROM publish_route WHERE binding_id = ?`, bindingID).
		Scan(&curBindingEpoch, &curAdapter, &curSession, &curLaunch, &launchVerified, &policyEpoch,
			&curMode, &modeAtMS, &curSocket, &curThread, &curState, &registeredAtMS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if curBindingEpoch != bindingEpoch || curAdapter != adapter || curSession == "" || sessionRef == "" || curSession != sessionRef ||
		curLaunch != launchRef || policyEpoch != epoch || curState == "ended" ||
		hookAtMS < registeredAtMS || hookAtMS <= modeAtMS {
		return nil
	}
	if newMode == curMode {
		_, err = tx.ExecContext(ctx, `UPDATE publish_route SET mode_at_ms = ?, updated_at_ms = ? WHERE binding_id = ?`,
			hookAtMS, ms(now), bindingID)
		return err
	}
	wasEligible := modeEligible(curAdapter, curMode)
	nowEligible := modeEligible(curAdapter, newMode)
	nowMS := ms(now)
	if wasEligible == nowEligible {
		// A proof is for one address: a mode that resolves another one
		// starts a new generation, which needs its own proof.
		next := int64(0)
		if a, ok := publish.Lookup(curAdapter); ok {
			route := publish.Route{SocketPath: curSocket.String, ThreadID: curThread.String}
			route.Mode = curMode
			before, _ := a.Resolve(route)
			route.Mode = newMode
			if after, _ := a.Resolve(route); after != before {
				next = 1
			}
		}
		_, err = tx.ExecContext(ctx, `UPDATE publish_route SET mode = ?, mode_at_ms = ?, generation = generation + ?, updated_at_ms = ?
			WHERE binding_id = ?`, newMode, hookAtMS, next, nowMS, bindingID)
		return err
	}
	if !nowEligible {
		_, err = tx.ExecContext(ctx, `UPDATE publish_route
			SET state = 'invalidated', reason = 'bypass mode', mode = ?, mode_at_ms = ?, generation = generation + 1, updated_at_ms = ?
			WHERE binding_id = ?`, newMode, hookAtMS, nowMS, bindingID)
		return err
	}
	newState, newReason := routeCandidateReason(curAdapter, launchVerified, newMode, curSocket.String, curThread.String)
	if newState != "active" {
		_, err = tx.ExecContext(ctx, `UPDATE publish_route
			SET state = ?, reason = ?, mode = ?, mode_at_ms = ?, updated_at_ms = ?
			WHERE binding_id = ?`, newState, newReason, newMode, hookAtMS, nowMS, bindingID)
		return err
	}
	watermark, err := currentWatermark(ctx, tx)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE publish_route
		SET state = 'active', reason = ?, mode = ?, mode_at_ms = ?, watermark_message_id = ?, generation = generation + 1, updated_at_ms = ?
		WHERE binding_id = ?`,
		newReason, newMode, hookAtMS, watermark, nowMS, bindingID)
	return err
}

func endPublishRoute(ctx context.Context, tx store.Tx, bindingID, bindingEpoch int64, launchRef, sessionRef string, epoch uint64, hookAtMS int64, reason string, now time.Time) error {
	if sessionRef == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE publish_route
		SET state = 'ended', reason = ?, generation = generation + 1, updated_at_ms = ?
		WHERE binding_id = ?
		  AND binding_epoch = ?
		  AND state <> 'ended'
		  AND launch_ref = ?
		  AND policy_epoch = ?
		  AND session_ref = ?
		  AND registered_at_ms <= ?`,
		reason, ms(now), bindingID, bindingEpoch, launchRef, epoch, sessionRef, hookAtMS)
	return err
}

func endLaunchPublishRoutes(ctx context.Context, tx store.Tx, principal int64, launchRef string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE publish_route
		SET state = 'ended', reason = 'launch ended', generation = generation + 1, updated_at_ms = ?
		WHERE principal_id = ? AND launch_ref = ? AND state <> 'ended'`, ms(now), principal, launchRef)
	return err
}

func retirePublishRoute(ctx context.Context, tx store.Tx, bindingID int64, reason string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE publish_route
		SET state = 'ended', reason = ?, generation = generation + 1, updated_at_ms = ?
		WHERE binding_id = ? AND state <> 'ended'`, reason, ms(now), bindingID)
	return err
}

func invalidatePublishRoutesForEpoch(ctx context.Context, tx store.Tx, adapter string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE publish_route
		SET state = 'invalidated', reason = 'epoch changed', generation = generation + 1, updated_at_ms = ?
		WHERE adapter = ? AND state = 'active'`, ms(now), adapter)
	return err
}

func invalidatePublishRouteForLeave(ctx context.Context, tx store.Tx, bindingID int64, now time.Time) error {
	return retirePublishRoute(ctx, tx, bindingID, "left channel", now)
}

// withPublishStatus gives an agent the state word and what will tell the
// member about new work; reasons and times are for the owner's views.
func withPublishStatus(ctx context.Context, tx store.Tx, bindingID int64, m *Member, now time.Time) error {
	status, found, err := store.ReadPublishState(ctx, tx, bindingID, now)
	if err != nil {
		return err
	}
	if found {
		m.Publish = &PublishStatus{State: status.State}
	}
	m.Wake = reachWake(status, found)
	return nil
}

func publishWord(status *PublishStatus) *PublishStatus {
	if status == nil {
		return nil
	}
	return &PublishStatus{State: status.State}
}
