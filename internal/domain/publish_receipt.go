package domain

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"strconv"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/notice"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

func observePublishReceipt(ctx context.Context, tx store.Tx, b hookBinding, principal int64, sessionKey string, cmd HookEvent, now time.Time) (bool, error) {
	ids := notice.ParseReceipt(cmd.Receipt)
	if ids == nil {
		return false, rejectPublishReceipt(ctx, tx, b.id, principal, "malformed", now)
	}
	if !cmd.LaunchVerified || cmd.Fact != FactPromptSubmitted || cmd.HookAtMS > ms(now)+futureSlackMS {
		return false, nil
	}
	var id, fenceAt int64
	var current bool
	sessionKey = resolveSessionKey(ctx, tx, sessionKey, principal, cmd.Profile.Adapter)
	err := tx.QueryRowContext(ctx, `SELECT a.id, a.submitting_at_ms,
		(r.state = 'active' AND r.generation = a.generation AND r.binding_epoch = a.binding_epoch
			AND r.adapter = a.adapter AND r.session_ref = a.session_ref AND r.launch_ref = a.launch_ref
			AND r.policy_epoch = a.policy_epoch AND a.launch_ref = ?)
		FROM publish_attempt a JOIN publish_route r ON r.binding_id = a.binding_id
		WHERE a.binding_id = ? AND a.principal_id = ? AND a.adapter = ? AND a.session_ref = ?
			AND a.observed_at_ms IS NULL AND a.retired_at_ms IS NULL AND a.state IN ('submitting', 'submitted', 'uncertain')`,
		cmd.LaunchRef, b.id, principal, cmd.Profile.Adapter, sessionKey).Scan(&id, &fenceAt, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	attempted, err := publishAttemptIDs(ctx, tx, id)
	if err != nil || !slices.Equal(attempted, ids) {
		return false, err
	}
	if !current || cmd.HookAtMS < fenceAt {
		return false, rejectPublishReceipt(ctx, tx, b.id, principal, "stale", now)
	}
	var observation string
	if err := tx.QueryRowContext(ctx, `WITH previous AS (
		SELECT t.turn_ref, t.finishing_hook_at_ms, t.opened_hook_at_ms, t.rowid FROM turn t JOIN adapter_launch l
			ON l.principal_id = t.principal_id AND l.launch_ref = t.launch_ref
		WHERE t.principal_id = ?1 AND t.launch_ref = ?2
			AND t.opened_hook_at_ms <= ?4 AND (t.opened_hook_at_ms >= l.started_hook_at_ms
				OR t.finishing_hook_at_ms BETWEEN l.started_hook_at_ms AND ?4)
	), last_turn AS (
		SELECT turn_ref, opened_hook_at_ms, finishing_hook_at_ms FROM previous ORDER BY opened_hook_at_ms DESC, rowid DESC LIMIT 1
	)
	SELECT CASE
		WHEN EXISTS (SELECT 1 FROM previous WHERE turn_ref = ?3) THEN 'absorbed'
		WHEN EXISTS (SELECT 1 FROM last_turn t WHERE t.finishing_hook_at_ms <= ?4 AND NOT EXISTS (
			SELECT 1 FROM observation o WHERE o.principal_id = ?1 AND o.launch_ref = ?2
				AND o.hook_at_ms BETWEEN t.finishing_hook_at_ms AND ?4
				AND (o.kind <> 'turn_finishing' OR o.turn_ref IS NOT t.turn_ref OR o.hook_at_ms <> t.finishing_hook_at_ms)
		)) THEN 'own_turn'
		-- A last turn with no Stop, and nothing observed since it opened but
		-- its own facts, ended by the time a prompt with another id arrives:
		-- Claude Code sends no Stop for a turn ended by a denied permission
		-- dialog (#169), and a prompt typed while a turn runs reports that
		-- turn's id, which the absorbed rule takes first. Any other turn's
		-- activity since (a prompt whose callback was lost) leaves it unknown.
		WHEN EXISTS (SELECT 1 FROM last_turn t WHERE t.finishing_hook_at_ms IS NULL AND NOT EXISTS (
			SELECT 1 FROM observation o WHERE o.principal_id = ?1 AND o.launch_ref = ?2
				AND o.hook_at_ms BETWEEN t.opened_hook_at_ms AND ?4 AND o.turn_ref IS NOT t.turn_ref
		)) THEN 'own_turn'
		WHEN NOT EXISTS (SELECT 1 FROM previous) AND NOT EXISTS (SELECT 1 FROM turn t
			WHERE t.principal_id = ?1 AND t.launch_ref = ?2 AND t.opened_hook_at_ms <= ?4
				AND (t.finishing_hook_at_ms IS NULL OR t.finishing_hook_at_ms > ?4)
		) AND EXISTS (SELECT 1 FROM adapter_launch l
			WHERE l.principal_id = ?1 AND l.launch_ref = ?2 AND l.started_hook_at_ms <= ?4 AND NOT EXISTS (
				SELECT 1 FROM observation o WHERE o.principal_id = ?1 AND o.launch_ref = ?2
					AND o.hook_at_ms BETWEEN l.started_hook_at_ms AND ?4
			)) THEN 'own_turn'
		ELSE 'unknown'
	END`, principal, cmd.LaunchRef, cmd.TurnRef, cmd.HookAtMS).Scan(&observation); err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE publish_attempt SET observed_at_ms = ?, observation = ?
		WHERE id = ? AND observed_at_ms IS NULL`, ms(now), observation, id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n != 1 {
		return false, err
	}
	if observation != "own_turn" {
		return false, suspendPublish(ctx, tx, id, observation, now)
	}
	return true, nil
}

func rejectPublishReceipt(ctx context.Context, tx store.Tx, binding, principal int64, reason string, now time.Time) error {
	return audit(ctx, tx, "publish_receipt_rejected", "binding:"+strconv.FormatInt(binding, 10), principal,
		map[string]string{"reason": reason}, now)
}
