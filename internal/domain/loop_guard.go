package domain

import (
	"context"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

const (
	roundDurationMS   = int64(10 * time.Minute / time.Millisecond)
	burstRequestLimit = 30
	burstAnswerLimit  = 60
	guardWouldRefuse  = "loop_guard_would_refuse"
)

type guardDecision struct {
	LevelMS     int64  `json:"level_ms"`
	Round       int64  `json:"round"`
	RoundLimit  int64  `json:"round_limit"`
	Requests    int64  `json:"requests"`
	OwedAnswers int64  `json:"owed_answers"`
	Path        int64  `json:"path_exchange_id,omitempty"`
	Reason      string `json:"reason"`
}

func evaluateGuard(now, position, limit, requests, owedAnswers int64, recipients int) guardDecision {
	level := max(now, position) + roundDurationMS
	decision := guardDecision{
		LevelMS: level, Round: roundAt(level, now),
		RoundLimit: limit, Requests: requests, OwedAnswers: owedAnswers,
	}
	switch {
	case recipients > burstAnswerLimit:
		decision.Reason = "audience_too_large"
	case requests > burstRequestLimit || owedAnswers > burstAnswerLimit:
		decision.Reason = "too_many_requests"
	case decision.Round > limit:
		decision.Reason = "round_limit"
	}
	return decision
}

func roundAt(level, now int64) int64 {
	if level <= now {
		return 0
	}
	return 1 + (level-now-1)/roundDurationMS
}

func guardPosition(ctx context.Context, tx store.Tx, principal, path, parent int64) (level, limit int64, err error) {
	if path == 0 {
		err = tx.QueryRowContext(ctx, `SELECT MAX(p.any_level_ms, COALESCE((
			SELECT MAX(m.level_ms) FROM obligation o JOIN message m ON m.id = o.message_id
			WHERE o.principal_id = p.id AND o.state = 'open' AND m.id > p.reset_after_message_id), 0)),
			p.round_limit FROM principal p WHERE p.id = ?`, principal).Scan(&level, &limit)
		return
	}
	err = tx.QueryRowContext(ctx, `SELECT MAX(COALESCE(p.level_ms, 0), COALESCE((
		SELECT MAX(m.level_ms) FROM obligation o JOIN message m ON m.id = o.message_id
		WHERE o.principal_id = ?1 AND o.state = 'open' AND m.exchange_id = e.id AND m.id > e.reset_after_message_id), 0),
		COALESCE((SELECT m.level_ms FROM message m WHERE m.id = ?3 AND m.exchange_id = e.id
			AND COALESCE(m.in_reply_to, m.id) > e.reset_after_message_id), 0)), e.round_limit
		FROM exchange e LEFT JOIN thread_position p ON p.exchange_id = e.id AND p.principal_id = ?1
		WHERE e.id = ?2`, principal, path, parent).Scan(&level, &limit)
	return
}

func guardForAsk(ctx context.Context, tx store.Tx, principal int64, attributed attribution, parent int64, recipients int, now time.Time) (guardDecision, error) {
	path := attributed.roundPath
	level, limit, err := guardPosition(ctx, tx, principal, path, parent)
	if err != nil {
		return guardDecision{}, err
	}
	if attributed.isNew && path != 0 {
		anyLevel, anyLimit, err := guardPosition(ctx, tx, principal, 0, 0)
		if err != nil {
			return guardDecision{}, err
		}
		if anyLevel > level || anyLevel == level && anyLimit < limit {
			path, level, limit = 0, anyLevel, anyLimit
		}
	}
	var requests, answers int64

	at := ms(now)
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(
		(SELECT COUNT(*) FROM obligation o WHERE o.message_id = m.id)), 0)
		FROM message m WHERE m.kind = 'ask' AND m.sender_id = ?
		AND m.created_at_ms > ? AND m.created_at_ms <= ?`, principal, at-roundDurationMS, at).
		Scan(&requests, &answers); err != nil {
		return guardDecision{}, err
	}
	decision := evaluateGuard(at, level, limit, requests+1, answers+int64(recipients), recipients)
	decision.Path = path
	return decision, nil
}

func foldRound(ctx context.Context, tx store.Tx, principal, origin, path, level int64) error {
	if level <= 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE principal SET any_level_ms = ?1
		WHERE id = ?2 AND reset_after_message_id < ?3 AND any_level_ms < ?1`, level, principal, origin); err != nil {
		return err
	}
	if path == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO thread_position (principal_id, exchange_id, level_ms)
		SELECT ?1, id, ?2 FROM exchange WHERE id = ?3 AND reset_after_message_id < ?4
		ON CONFLICT (principal_id, exchange_id) DO UPDATE SET level_ms = excluded.level_ms
		WHERE thread_position.level_ms < excluded.level_ms`, principal, level, path, origin)
	return err
}
