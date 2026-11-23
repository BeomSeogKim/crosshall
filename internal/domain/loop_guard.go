package domain

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

const (
	roundDurationMS    = int64(10 * time.Minute / time.Millisecond)
	burstRequestLimit  = 30
	burstAnswerLimit   = 60
	guardDefaultRounds = 8
	guardStopped       = "loop_guard_stopped"
)

type guardDecision struct {
	newThreadLimit int64
	BaseMS         int64
	LevelMS        int64  `json:"level_ms"`
	Round          int64  `json:"round"`
	RoundLimit     int64  `json:"round_limit"`
	Requests       int64  `json:"requests"`
	OwedAnswers    int64  `json:"owed_answers"`
	Path           int64  `json:"thread_root_id,omitempty"`
	Reason         string `json:"reason"`
}

func evaluateGuard(now, position, limit, requests, owedAnswers int64, recipients int) guardDecision {
	level := max(now, position) + roundDurationMS
	decision := guardDecision{
		BaseMS: position, LevelMS: level, Round: roundAt(level, now),
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

type guardStop struct {
	Channel      string   `json:"channel"`
	Sender       string   `json:"sender"`
	Recipients   []string `json:"recipients"`
	Round        int64    `json:"round"`
	RoundLimit   int64    `json:"round_limit"`
	ThreadRootID int64    `json:"thread_root_id"`
	DrainsAtMS   int64    `json:"drains_at_ms"`
}

func refuseGuard(ctx context.Context, tx store.Tx, m member, cmd Ask, targets []target, d guardDecision, now time.Time) (core.Result, error) {
	switch d.Reason {
	case "audience_too_large":
		return Refuse(core.CodeAudienceTooLarge,
			fmt.Sprintf("not sent: %d members would owe an answer; one request may ask at most %d", len(targets), burstAnswerLimit),
			"name fewer members, or send a note."), nil
	case "too_many_requests":
		at, err := burstRetryAt(ctx, tx, m.principal, len(targets), now)
		if err != nil {
			return core.Result{}, err
		}
		description := fmt.Sprintf("your requests in the last 10 minutes already ask for %d answers; this one adds %d and the limit is %d",
			d.OwedAnswers-int64(len(targets)), len(targets), burstAnswerLimit)
		if d.Requests > burstRequestLimit {
			description = fmt.Sprintf("%d requests from you in 10 minutes", d.Requests-1)
		}
		return Refuse(core.CodeTooManyRequests, "not sent: "+description,
			fmt.Sprintf("it fits the burst limit again at %s. Nothing else lifts this.", at.Format("15:04"))), nil
	}

	const guidance = "do not retry or reroute it. crosshall reply and crosshall note still work. The stop is recorded for the owner."
	detail := guardStop{Channel: m.channelName, Sender: m.alias, Recipients: sortedUnique(cmd.To),
		Round: d.Round, RoundLimit: d.RoundLimit, ThreadRootID: d.Path, DrainsAtMS: d.BaseMS}
	if cmd.All {
		for _, t := range targets {
			detail.Recipients = append(detail.Recipients, t.alias)
		}
	}
	if d.Path != 0 {
		exchange, err := exchangeForRoot(ctx, tx, d.Path)
		if err != nil {
			return core.Result{}, err
		}
		updated, err := tx.ExecContext(ctx, `UPDATE exchange SET episode_open = 1, episode_count = episode_count + 1
			WHERE id = ? AND episode_open = 0`, exchange)
		if err := recordGuardStop(ctx, tx, updated, err, fmt.Sprintf("exchange:%d", exchange), m.principal, detail, now); err != nil {
			return core.Result{}, err
		}
		return Refuse(core.CodeRoundLimit,
			fmt.Sprintf("not sent: this request would be round %d on x%d; this thread may run %d rounds ahead", d.Round, exchange, d.RoundLimit),
			guidance), nil
	}
	hint, err := guardParentHint(ctx, tx, m, ms(now))
	if err != nil {
		return core.Result{}, err
	}
	updated, err := tx.ExecContext(ctx, `UPDATE principal SET guard_told = 1 WHERE id = ? AND guard_told = 0`, m.principal)
	if err := recordGuardStop(ctx, tx, updated, err, fmt.Sprintf("principal:%d", m.principal), m.principal, detail, now); err != nil {
		return core.Result{}, err
	}
	return Refuse(core.CodeRoundLimit,
		fmt.Sprintf("not sent: this request would be round %d across what you have picked up; new threads from you may run %d rounds ahead",
			d.Round, d.RoundLimit), guidance+hint), nil
}

func burstRetryAt(ctx context.Context, tx store.Tx, principal int64, recipients int, now time.Time) (time.Time, error) {
	rows, err := tx.QueryContext(ctx, `SELECT m.created_at_ms, (SELECT COUNT(*) FROM obligation o WHERE o.message_id = m.id)
		FROM message m WHERE m.kind = 'ask' AND m.sender_id = ?
		AND m.created_at_ms > ? AND m.created_at_ms <= ? ORDER BY m.created_at_ms DESC`, principal, ms(now)-roundDurationMS, ms(now))
	if err != nil {
		return time.Time{}, err
	}
	defer rows.Close()
	requests, answers := 1, recipients
	for rows.Next() {
		var at int64
		var owed int
		if err := rows.Scan(&at, &owed); err != nil {
			return time.Time{}, err
		}
		requests++
		answers += owed
		if requests > burstRequestLimit || answers > burstAnswerLimit {
			retry := time.UnixMilli(at + roundDurationMS)
			ceiling := retry.Truncate(time.Minute)
			if ceiling.Before(retry) {
				ceiling = ceiling.Add(time.Minute)
			}
			return ceiling, nil
		}
	}
	if err := rows.Err(); err != nil {
		return time.Time{}, err
	}
	return time.Time{}, fmt.Errorf("domain: burst refusal has no expiring request")
}

func guardParentHint(ctx context.Context, tx store.Tx, m member, now int64) (string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT ask.id, COALESCE(ask.thread_root_id, ask.id)
		FROM obligation o JOIN message ask ON ask.id = o.message_id
		WHERE o.principal_id = ? AND o.state = 'open' AND ask.channel_id = ? ORDER BY ask.id`, m.principal, m.channel)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var ids []string
	for len(ids) < 5 && rows.Next() {
		var id, root int64
		if err := rows.Scan(&id, &root); err != nil {
			return "", err
		}
		level, limit, err := guardPosition(ctx, tx, m.principal, root, id)
		if err != nil {
			return "", err
		}
		if roundAt(max(now, level)+roundDurationMS, now) <= limit {
			ids = append(ids, strconv.FormatInt(id, 10))
		}
	}
	if err := rows.Err(); err != nil || len(ids) == 0 {
		return "", err
	}
	return " If it follows a request you owe, add --re <id>: " + strings.Join(ids, " ") + ".", nil
}

func recordGuardStop(ctx context.Context, tx store.Tx, updated sql.Result, err error, subject string, principal int64, detail guardStop, now time.Time) error {
	if err != nil {
		return err
	}
	changed, err := updated.RowsAffected()
	if err != nil || changed == 0 {
		return err
	}
	return audit(ctx, tx, guardStopped, subject, principal, detail, now)
}

func endDrainedStop(ctx context.Context, tx store.Tx, principal, threadRoot, base, now int64) error {
	if base > now {
		return nil
	}
	if threadRoot != 0 {
		_, err := tx.ExecContext(ctx, `UPDATE exchange SET episode_open = 0 WHERE episode_open = 1
			AND id = (SELECT exchange_id FROM message WHERE id = ?1)
			AND (SELECT json_extract(detail, '$.thread_root_id') = ?1
				AND COALESCE(json_extract(detail, '$.drains_at_ms'), 0) <= ?3 FROM event
				WHERE kind = ?2 AND subject = 'exchange:' || exchange.id ORDER BY seq DESC LIMIT 1)`, threadRoot, guardStopped, now)
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE principal SET guard_told = 0 WHERE id = ?1 AND guard_told = 1
		AND (SELECT COALESCE(json_extract(detail, '$.drains_at_ms'), 0) <= ?3 FROM event
			WHERE kind = ?2 AND subject = 'principal:' || ?1 ORDER BY seq DESC LIMIT 1)`, principal, guardStopped, now)
	return err
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
		WHERE o.principal_id = ?1 AND o.state = 'open' AND COALESCE(m.thread_root_id, m.id) = root.id
			AND m.id > root.thread_reset_after_message_id), 0),
		COALESCE((SELECT m.level_ms FROM message m WHERE m.id = ?3
			AND COALESCE(m.in_reply_to, m.id) > root.thread_reset_after_message_id), 0)), root.thread_round_limit
		FROM message root LEFT JOIN thread_position p ON p.thread_root_id = root.id AND p.principal_id = ?1
		WHERE root.id = ?2`, principal, path, parent).Scan(&level, &limit)
	return
}

func guardAskPath(ctx context.Context, tx store.Tx, m member, cmd Ask) (int64, error) {
	if cmd.Re != 0 {
		var root int64
		err := tx.QueryRowContext(ctx, `SELECT COALESCE(thread_root_id, in_reply_to, id) FROM message WHERE id = ?`, cmd.Re).Scan(&root)
		return root, err
	}
	if cmd.New {
		return 0, nil
	}
	var first, last int64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(MIN(COALESCE(m.thread_root_id, m.id)), 0),
		COALESCE(MAX(COALESCE(m.thread_root_id, m.id)), 0)
		FROM obligation o JOIN message m ON m.id = o.message_id
		WHERE o.principal_id = ? AND o.state = 'open' AND m.channel_id = ?`, m.principal, m.channel).Scan(&first, &last)
	if first == last {
		return first, err
	}
	return 0, err
}

func guardForAsk(ctx context.Context, tx store.Tx, m member, cmd Ask, recipients int, now time.Time) (guardDecision, error) {
	path, err := guardAskPath(ctx, tx, m, cmd)
	if err != nil {
		return guardDecision{}, err
	}
	level, limit, err := guardPosition(ctx, tx, m.principal, path, cmd.Re)
	if err != nil {
		return guardDecision{}, err
	}
	newThreadLimit := limit
	if cmd.New && path != 0 {
		anyLevel, anyLimit, err := guardPosition(ctx, tx, m.principal, 0, 0)
		if err != nil {
			return guardDecision{}, err
		}
		newThreadLimit = anyLimit
		if anyLimit < limit || anyLimit == limit && anyLevel > level {
			path = 0
		}
		level, limit = max(level, anyLevel), min(limit, anyLimit)
	}
	var requests, answers int64

	at := ms(now)
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(
		(SELECT COUNT(*) FROM obligation o WHERE o.message_id = m.id)), 0)
		FROM message m WHERE m.kind = 'ask' AND m.sender_id = ?
		AND m.created_at_ms > ? AND m.created_at_ms <= ?`, m.principal, at-roundDurationMS, at).
		Scan(&requests, &answers); err != nil {
		return guardDecision{}, err
	}
	decision := evaluateGuard(at, level, limit, requests+1, answers+int64(recipients), recipients)
	decision.Path = path
	decision.newThreadLimit = newThreadLimit
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
	_, err := tx.ExecContext(ctx, `INSERT INTO thread_position (principal_id, thread_root_id, level_ms)
		SELECT ?1, id, ?2 FROM message WHERE id = ?3 AND thread_reset_after_message_id < ?4
		ON CONFLICT (principal_id, thread_root_id) DO UPDATE SET level_ms = excluded.level_ms
		WHERE thread_position.level_ms < excluded.level_ms`, principal, level, path, origin)
	return err
}
