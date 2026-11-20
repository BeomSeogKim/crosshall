package domain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

const VerbHookRoute = wire.VerbHookRoute

// HookRoute registers or updates a member's publish route from a hook.
type HookRoute struct{ wire.HookRouteArgs }

func (HookRoute) command()     {}
func (HookRoute) Verb() string { return VerbHookRoute }

// PublishStatus describes a member's publish route state in who output.
// In slice 1, routes are never available for sending.
type PublishStatus struct {
	State      string `json:"state"`
	Reason     string `json:"reason,omitempty"`
	Generation int64  `json:"generation,omitempty"`
}

func allowedClaudeSocketDirs() []string {
	uid := os.Getuid()
	bases := []string{"/tmp"}
	if runtime.GOOS == "darwin" {
		bases = append(bases, "/private/tmp")
	}
	if tmp := os.TempDir(); tmp != "" && filepath.IsAbs(tmp) {
		bases = append(bases, filepath.Clean(tmp))
	}
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" && filepath.IsAbs(xdg) {
		bases = append(bases, filepath.Clean(xdg))
	}
	var dirs []string
	for _, base := range bases {
		dirs = append(dirs,
			filepath.Join(base, "cc-socks"),
			filepath.Join(base, fmt.Sprintf("cc-socks-%d", uid)),
		)
	}
	return dirs
}

func validClaudeSocketPath(path string) bool {
	if path == "" || len(path) > 512 || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return false
	}
	if ValidText("socket path", path) != "" {
		return false
	}
	if strings.IndexFunc(path, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return false
	}
	sock := filepath.Base(path)
	if sock == ".sock" || !strings.HasSuffix(sock, ".sock") {
		return false
	}
	return slices.Contains(allowedClaudeSocketDirs(), filepath.Dir(path))
}

func modeEligible(adapter, mode string) bool {
	if adapter == "claude" && (mode == "bypassPermissions" || mode == "bypass") {
		return false
	}
	return true
}

func routeComplete(adapter, socketPath, threadID string) (bool, string) {
	switch adapter {
	case "claude":
		if socketPath == "" {
			return false, "no socket"
		}
		return true, ""
	case "codex":
		if threadID == "" {
			return false, "no thread"
		}
		return true, ""
	default:
		return false, "unsupported adapter"
	}
}

func routeCandidateReason(adapter string, launchVerified bool, mode, socketPath, threadID string) (string, string) {
	if complete, why := routeComplete(adapter, socketPath, threadID); !complete {
		return "invalidated", why
	}
	if !modeEligible(adapter, mode) {
		return "invalidated", "bypass mode"
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

func registerPublishRouteFromEnroll(ctx context.Context, tx store.Tx, bindingID, principal int64, sessionKey string, cmd HookEnroll, now time.Time) error {
	adapter := cmd.Profile.Adapter
	if adapter != "claude" && adapter != "codex" {
		return nil
	}
	sessionRef := resolveSessionKey(ctx, tx, sessionKey, principal, adapter)
	if sessionRef == "" {
		return nil
	}
	var threadID string
	if adapter == "codex" {
		threadID = sessionRef
	}
	// On Claude SessionStart, hook.enroll carries no socket path; hook.route supplies it.
	return upsertPublishRoute(ctx, tx, bindingID, principal, adapter, sessionRef, cmd.LaunchRef, cmd.LaunchVerified,
		cmd.Epoch, "", "", threadID, cmd.HookAtMS, now)
}

func upsertPublishRoute(ctx context.Context, tx store.Tx, bindingID, principal int64, adapter, sessionRef, launchRef string,
	launchVerified bool, epoch uint64, mode, socketPath, threadID string, hookAtMS int64, now time.Time) error {
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
		binding_id, principal_id, generation, adapter, session_ref, launch_ref,
		launch_verified, policy_epoch, mode, mode_at_ms, socket_path, thread_id,
		watermark_message_id, state, reason, registered_at_ms, updated_at_ms
	) VALUES (?, ?, 1, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?, ?)
	ON CONFLICT(binding_id) DO UPDATE SET
		principal_id = excluded.principal_id,
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
		watermark_message_id = excluded.watermark_message_id,
		state = excluded.state,
		reason = excluded.reason,
		registered_at_ms = excluded.registered_at_ms,
		updated_at_ms = excluded.updated_at_ms
	WHERE excluded.registered_at_ms >= publish_route.registered_at_ms`,
		bindingID, principal, adapter, sessionRef, launchRef,
		launchVerified, epoch, mode, modeAtMS, socketPath, threadID,
		watermark, state, reason, hookAtMS, nowMS)
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
	if cmd.SocketPath != "" && (cmd.Adapter != "claude" || !validClaudeSocketPath(cmd.SocketPath)) {
		return Refuse("", "socket path is outside the vendor's socket directory", "use the session's vendor socket path"), nil
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
	if cmd.SocketPath != "" {
		if err := upsertPublishRoute(ctx, tx, b.id, principal, cmd.Adapter, sessionRef, cmd.LaunchRef, cmd.LaunchVerified,
			cmd.Epoch, cmd.PermissionMode, cmd.SocketPath, "", cmd.HookAtMS, now); err != nil {
			return core.Result{}, err
		}
		return OK(out)
	}
	if cmd.PermissionMode != "" {
		if err := updatePublishRouteMode(ctx, tx, b.id, cmd.Adapter, sessionRef, cmd.LaunchRef, cmd.Epoch, cmd.PermissionMode, cmd.HookAtMS, now); err != nil {
			return core.Result{}, err
		}
	}
	return OK(out)
}

func updatePublishRouteMode(ctx context.Context, tx store.Tx, bindingID int64, adapter, sessionRef, launchRef string,
	epoch uint64, newMode string, hookAtMS int64, now time.Time) error {
	var (
		curAdapter, curSession, curLaunch, curMode, curState string
		curSocket, curThread                                 sql.NullString
		launchVerified                                       bool
		policyEpoch                                          uint64
		modeAtMS, registeredAtMS                             int64
	)
	err := tx.QueryRowContext(ctx, `SELECT adapter, session_ref, launch_ref, launch_verified, policy_epoch,
		mode, mode_at_ms, socket_path, thread_id, state, registered_at_ms
		FROM publish_route WHERE binding_id = ?`, bindingID).
		Scan(&curAdapter, &curSession, &curLaunch, &launchVerified, &policyEpoch,
			&curMode, &modeAtMS, &curSocket, &curThread, &curState, &registeredAtMS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if curAdapter != adapter || curSession == "" || sessionRef == "" || curSession != sessionRef ||
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
		_, err = tx.ExecContext(ctx, `UPDATE publish_route SET mode = ?, mode_at_ms = ?, updated_at_ms = ? WHERE binding_id = ?`,
			newMode, hookAtMS, nowMS, bindingID)
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

func endPublishRoute(ctx context.Context, tx store.Tx, bindingID int64, launchRef, sessionRef string, epoch uint64, hookAtMS int64, reason string, now time.Time) error {
	if sessionRef == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE publish_route
		SET state = 'ended', reason = ?, generation = generation + 1, updated_at_ms = ?
		WHERE binding_id = ?
		  AND state <> 'ended'
		  AND launch_ref = ?
		  AND policy_epoch = ?
		  AND session_ref = ?
		  AND registered_at_ms <= ?`,
		reason, ms(now), bindingID, launchRef, epoch, sessionRef, hookAtMS)
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

func withPublishStatus(ctx context.Context, tx store.Tx, bindingID int64, vendor string, m *Member) error {
	var (
		state, reason string
		generation    int64
	)
	err := tx.QueryRowContext(ctx, `SELECT state, reason, generation FROM publish_route WHERE binding_id = ?`, bindingID).
		Scan(&state, &reason, &generation)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if state == "active" {
		m.Publish = &PublishStatus{State: "candidate", Reason: reason, Generation: generation}
	} else {
		m.Publish = &PublishStatus{State: "off", Reason: reason, Generation: generation}
	}
	return nil
}
