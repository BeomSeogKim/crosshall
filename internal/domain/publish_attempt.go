package domain

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

type PublishBinding struct {
	ID         int64
	Generation int64
}

type PublishQueue struct {
	PID     int64
	StartMS int64
}

type PublishTarget struct {
	Adapter string
	Address string
	Queue   PublishQueue
}

type PublishOutcome string

const (
	PublishNotSubmitted PublishOutcome = "not_submitted"
	PublishSubmitted    PublishOutcome = "submitted"
	PublishUncertain    PublishOutcome = "uncertain"
)

const activePublishRoutes = `FROM publish_route r
	JOIN binding b ON b.id = r.binding_id AND b.principal_id = r.principal_id AND b.route_epoch = r.binding_epoch
	JOIN publish_route_proof p ON p.binding_id = r.binding_id AND p.generation = r.generation
	JOIN adapter_policy policy ON policy.adapter = r.adapter AND policy.epoch = r.policy_epoch AND policy.enabled = 1
	JOIN adapter_launch l ON l.rowid = (SELECT current.rowid FROM adapter_launch current
		WHERE current.principal_id = r.principal_id AND current.ended_hook_at_ms IS NULL
		ORDER BY current.started_hook_at_ms DESC, current.rowid DESC LIMIT 1)
	WHERE r.adapter = 'claude' AND r.state = 'active' AND r.socket_path IS NOT NULL
		AND r.launch_verified = 1 AND l.launch_ref = r.launch_ref AND l.verified = 1
		AND b.state = 'live' AND b.dnd = 0
		AND ` + store.PublishSwitchesOn + ` AND ` + store.PublishNotSuspended + ` AND ` + store.PublishProfileTested

const publishMessages = `SELECT x.message_id FROM recipient x JOIN message m ON m.id = x.message_id
	WHERE x.principal_id = r.principal_id AND m.channel_id = b.channel_id
		AND x.message_id > r.watermark_message_id AND x.picked_up_at_ms IS NULL
		AND (m.kind = 'ask' AND EXISTS (SELECT 1 FROM obligation o
			WHERE o.message_id = x.message_id AND o.principal_id = x.principal_id AND o.state = 'open')
			OR m.kind = 'reply' AND m.outcome IN ('answer', 'decline') AND EXISTS (
				SELECT 1 FROM message ask WHERE ask.id = m.in_reply_to AND ask.kind = 'ask' AND ask.sender_id = x.principal_id))
		AND NOT EXISTS (SELECT 1 FROM announcement a WHERE a.principal_id = x.principal_id AND a.message_id = x.message_id)
		AND NOT EXISTS (SELECT 1 FROM publish_attempt_item i JOIN publish_attempt a ON a.id = i.attempt_id
			WHERE i.principal_id = x.principal_id AND i.message_id = x.message_id AND a.binding_id = r.binding_id
				AND a.generation = r.generation AND a.state = 'not_submitted')`

const openPublishLane = `observed_at_ms IS NULL AND retired_at_ms IS NULL AND state <> 'not_submitted'`

// ConfirmPublishRoute records a transport's positive proof for this generation.
func ConfirmPublishRoute(ctx context.Context, tx store.Tx, binding PublishBinding, queue PublishQueue) error {
	if _, err := tx.ExecContext(ctx, `UPDATE publish_route AS r SET watermark_message_id = (SELECT COALESCE(MAX(id), 0) FROM message)
		WHERE r.binding_id = ? AND r.generation = ? AND r.state = 'active' AND r.launch_verified = 1
			AND NOT EXISTS (SELECT 1 FROM publish_route_proof p WHERE p.binding_id = r.binding_id AND
				(p.generation >= r.generation OR (p.process_pid = ? AND p.process_start_ms = ?)))`,
		binding.ID, binding.Generation, queue.PID, queue.StartMS); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO publish_route_proof (binding_id, generation, process_pid, process_start_ms)
		SELECT binding_id, generation, ?1, ?2 FROM publish_route
		WHERE binding_id = ?3 AND generation = ?4 AND state = 'active' AND launch_verified = 1
		ON CONFLICT(binding_id) DO UPDATE SET generation = excluded.generation,
			process_pid = excluded.process_pid, process_start_ms = excluded.process_start_ms
		WHERE publish_route_proof.generation < excluded.generation`,
		queue.PID, queue.StartMS, binding.ID, binding.Generation)
	return err
}

func PublishCandidates(ctx context.Context, tx store.Tx) ([]PublishBinding, error) {
	rows, err := tx.QueryContext(ctx, `SELECT r.binding_id, r.generation `+activePublishRoutes+
		` AND EXISTS (`+publishMessages+`) ORDER BY r.binding_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var bindings []PublishBinding
	for rows.Next() {
		var binding PublishBinding
		if err := rows.Scan(&binding.ID, &binding.Generation); err != nil {
			return nil, err
		}
		bindings = append(bindings, binding)
	}
	return bindings, rows.Err()
}

func HasPublishWork(ctx context.Context, tx store.Tx, binding PublishBinding) (bool, error) {
	var pending bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 `+activePublishRoutes+
		` AND r.binding_id = ? AND r.generation = ? AND EXISTS (`+publishMessages+`))`,
		binding.ID, binding.Generation).Scan(&pending)
	return pending, err
}

// ReservePublish reads the route and unread ledger again under the writer transaction.
func ReservePublish(ctx context.Context, tx store.Tx, binding PublishBinding, now time.Time) (bool, error) {
	res, err := tx.ExecContext(ctx, `INSERT INTO publish_attempt (
		binding_id, principal_id, binding_epoch, generation, adapter, session_ref, launch_ref,
		policy_epoch, process_pid, process_start_ms, state, reserved_at_ms)
		SELECT r.binding_id, r.principal_id, r.binding_epoch, r.generation, r.adapter, r.session_ref, r.launch_ref,
			r.policy_epoch, p.process_pid, p.process_start_ms, 'reserved', ?1 `+activePublishRoutes+`
		AND r.binding_id = ?2 AND r.generation = ?3
		AND EXISTS (`+publishMessages+`)
		AND (SELECT COUNT(*) FROM publish_attempt a WHERE a.principal_id = r.principal_id AND
			(a.state = 'reserved' OR (a.submitting_at_ms > ?4 AND a.state IN ('submitting', 'submitted', 'uncertain')))) < ?5
		ON CONFLICT DO NOTHING`, ms(now), binding.ID, binding.Generation, ms(now.Add(-time.Hour)), store.PublishHourlyCap)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO publish_attempt_item (attempt_id, principal_id, message_id)
		SELECT ?1, r.principal_id, elig.message_id FROM publish_route r JOIN binding b ON b.id = r.binding_id
		JOIN recipient elig ON elig.principal_id = r.principal_id
		WHERE r.binding_id = ?2 AND elig.message_id IN (`+publishMessages+` ORDER BY x.message_id LIMIT ?3)`,
		id, binding.ID, OfferCap); err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO announcement (principal_id, message_id, via, ref, at_ms)
		SELECT principal_id, message_id, 'publish', attempt_id, ? FROM publish_attempt_item WHERE attempt_id = ?`, ms(now), id)
	return err == nil, err
}

func ReservedPublishAttempts(ctx context.Context, tx store.Tx) ([]int64, error) {
	return selectIDs(ctx, tx, `SELECT id FROM publish_attempt WHERE state = 'reserved' ORDER BY id`)
}

func publishAttemptIDs(ctx context.Context, tx store.Tx, id int64) ([]int64, error) {
	return selectIDs(ctx, tx, `SELECT message_id FROM publish_attempt_item WHERE attempt_id = ? ORDER BY message_id`, id)
}

// BeginPublish is the final fence. Only its winner may call the transport.
func BeginPublish(ctx context.Context, tx store.Tx, id int64, now time.Time) (PublishTarget, []int64, error) {
	var target PublishTarget
	err := tx.QueryRowContext(ctx, `SELECT r.adapter, r.socket_path, p.process_pid, p.process_start_ms `+
		activePublishRoutes+` AND EXISTS (SELECT 1 FROM publish_attempt a WHERE a.id = ? AND a.state = 'reserved'
			AND a.binding_id = r.binding_id AND a.principal_id = r.principal_id AND a.binding_epoch = r.binding_epoch
			AND a.generation = r.generation AND a.adapter = r.adapter AND a.session_ref = r.session_ref AND a.launch_ref = r.launch_ref
			AND a.policy_epoch = r.policy_epoch AND a.process_pid = p.process_pid AND a.process_start_ms = p.process_start_ms)`, id).
		Scan(&target.Adapter, &target.Address, &target.Queue.PID, &target.Queue.StartMS)
	if errors.Is(err, sql.ErrNoRows) {
		return PublishTarget{}, nil, finishPublish(ctx, tx, id, "reserved", PublishNotSubmitted, now)
	}
	if err != nil {
		return PublishTarget{}, nil, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE publish_attempt SET state = 'submitting', submitting_at_ms = ?
		WHERE id = ? AND state = 'reserved'`, ms(now), id)
	if err != nil {
		return PublishTarget{}, nil, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return PublishTarget{}, nil, err
	}
	ids, err := publishAttemptIDs(ctx, tx, id)
	return target, ids, err
}

func ResolvePublish(ctx context.Context, tx store.Tx, id int64, outcome PublishOutcome, now time.Time) error {
	switch outcome {
	case PublishNotSubmitted, PublishSubmitted, PublishUncertain:
	default:
		outcome = PublishUncertain
	}
	return finishPublish(ctx, tx, id, "submitting", outcome, now)
}

func finishPublish(ctx context.Context, tx store.Tx, id int64, from string, outcome PublishOutcome, now time.Time) error {
	res, err := tx.ExecContext(ctx, `UPDATE publish_attempt SET state = ?, resolved_at_ms = ?
		WHERE id = ? AND state = ?`, outcome, ms(now), id, from)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 || outcome != PublishNotSubmitted {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM announcement WHERE via = 'publish' AND ref = ?`, id)
	return err
}

func RecoverPublish(ctx context.Context, tx store.Tx, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE publish_attempt SET state = 'uncertain', resolved_at_ms = ?
		WHERE state = 'submitting'`, ms(now))
	return err
}

func HeldPublishQueues(ctx context.Context, tx store.Tx) ([]PublishQueue, error) {
	rows, err := tx.QueryContext(ctx, `SELECT process_pid, process_start_ms FROM publish_attempt WHERE `+openPublishLane)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var queues []PublishQueue
	for rows.Next() {
		var queue PublishQueue
		if err := rows.Scan(&queue.PID, &queue.StartMS); err != nil {
			return nil, err
		}
		queues = append(queues, queue)
	}
	return queues, rows.Err()
}

// RetirePublishQueue is called only after the transport proves the queue is gone.
func RetirePublishQueue(ctx context.Context, tx store.Tx, queue PublishQueue, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM publish_route_proof WHERE process_pid = ? AND process_start_ms = ?`,
		queue.PID, queue.StartMS); err != nil {
		return err
	}
	ids, err := selectIDs(ctx, tx, `SELECT id FROM publish_attempt WHERE process_pid = ? AND process_start_ms = ? AND state = 'reserved'`,
		queue.PID, queue.StartMS)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := finishPublish(ctx, tx, id, "reserved", PublishNotSubmitted, now); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE publish_attempt SET retired_at_ms = ? WHERE process_pid = ? AND process_start_ms = ? AND `+
		openPublishLane, ms(now), queue.PID, queue.StartMS)
	return err
}
