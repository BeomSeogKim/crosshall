package domain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// Reply outcomes.
const (
	OutcomeAnswer   = "answer"
	OutcomeDecline  = "decline"
	OutcomeMoreTime = "more_time"
)

// Reply answers one request, declines it, or asks for more time.
type Reply struct {
	To      int64  `json:"to"`
	Body    string `json:"body,omitempty"`
	Outcome string `json:"outcome,omitempty"`
}

func (Reply) command() {}

// Verb returns the wire verb.
func (Reply) Verb() string { return VerbReply }

func (r Reply) normalized() Command {
	if r.Outcome == "" {
		r.Outcome = OutcomeAnswer
	}
	return r
}

// ReplyResult is the data of a committed reply.
type ReplyResult struct {
	ID         int64  `json:"id"`
	Channel    string `json:"channel"`
	Ask        int64  `json:"ask"`
	Outcome    string `json:"outcome"`
	To         string `json:"to"`
	DueAtMS    int64  `json:"due_at_ms"`
	LateByMS   int64  `json:"late_by_ms,omitempty"`
	Obligation string `json:"obligation"`
	// A final response may settle the request before its recipient reads it.
	OriginalUnread bool `json:"original_unread,omitempty"`
	// Delivery is the reply's own state on its way to the asker, current as
	// of this response (also on a replay).
	Delivery Delivery `json:"delivery"`
}

func checkReply(cmd Reply) *core.Result {
	usage := func(msg string) *core.Result {
		r := Fail(core.ExitUsage, "", msg, "crosshall help reply")
		return &r
	}
	cmd = cmd.normalized().(Reply)
	switch {
	case cmd.To <= 0:
		return usage("name the request to answer: crosshall reply <id> \"...\"")
	case cmd.Outcome != OutcomeAnswer && cmd.Outcome != OutcomeDecline && cmd.Outcome != OutcomeMoreTime:
		return usage(fmt.Sprintf("unknown reply outcome %q", cmd.Outcome))
	case cmd.Outcome == OutcomeAnswer && strings.TrimSpace(cmd.Body) == "":
		return usage("the answer has no text")
	case !utf8.ValidString(cmd.Body):
		return usage("the text is not valid UTF-8")
	case len(cmd.Body) > MaxBody:
		return usage(fmt.Sprintf("the text is longer than %d bytes; point at a file instead", MaxBody))
	}
	return nil
}

func (s *Service) reply(ctx context.Context, tx store.Tx, caller core.Caller, req Request, cmd Reply, now time.Time) (core.Result, error) {
	m, refusal, err := liveMember(ctx, tx, caller)
	if err != nil || refusal != nil {
		return deref(refusal), err
	}
	if r := checkReply(cmd); r != nil {
		return *r, nil
	}
	cmd = cmd.normalized().(Reply)
	scope := "channel:" + m.channelName
	if r, found, err := replay(ctx, tx, m.principal, req.OpKey, scope, cmd); err != nil || found {
		if err != nil || r.Status != core.ExitOK {
			return r, err
		}
		return withCurrentReplyDelivery(ctx, tx, r, m.principal, &s.waits, now)
	}

	var (
		asker      int64
		exchange   sql.NullInt64
		due        int64
		level      int64
		threadRoot int64
	)
	err = tx.QueryRowContext(ctx,
		`SELECT sender_id, exchange_id, due_at_ms, level_ms, COALESCE(thread_root_id, id)
		 FROM message WHERE id = ? AND channel_id = ? AND kind = 'ask'`,
		cmd.To, m.channel).Scan(&asker, &exchange, &due, &level, &threadRoot)
	if errors.Is(err, sql.ErrNoRows) {
		return Refuse(core.CodeNotFound, fmt.Sprintf("no request #%d in #%s", cmd.To, m.channelName), "crosshall inbox"), nil
	}
	if err != nil {
		return core.Result{}, err
	}
	var state string
	var unread bool
	err = tx.QueryRowContext(ctx,
		`SELECT o.state, r.picked_up_at_ms IS NULL FROM obligation o
		 JOIN recipient r USING (message_id, principal_id)
		 WHERE o.message_id = ? AND o.principal_id = ?`, cmd.To, m.principal).Scan(&state, &unread)
	if errors.Is(err, sql.ErrNoRows) {
		return Refuse(core.CodeNotObligated, fmt.Sprintf("#%d was not addressed to you", cmd.To), "crosshall inbox"), nil
	}
	if err != nil {
		return core.Result{}, err
	}
	switch state {
	case "replied", "declined":
		return Refuse(core.CodeAlreadySettled, fmt.Sprintf("you already %s #%d", state, cmd.To), "it needs no further reply; to add something, use crosshall note"), nil
	case "cancelled":
		return Refuse(core.CodeCancelled, fmt.Sprintf("the owner cancelled #%d", cmd.To), "no answer is owed; crosshall inbox"), nil
	}

	var askerAlias string
	if err := tx.QueryRowContext(ctx,
		`SELECT alias FROM binding WHERE principal_id = ? AND channel_id = ? ORDER BY id DESC LIMIT 1`,
		asker, m.channel).Scan(&askerAlias); err != nil {
		return core.Result{}, err
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO message (channel_id, sender_id, kind, outcome, in_reply_to, exchange_id, audience_mode, body, created_at_ms, level_ms, thread_root_id)
		 VALUES (?, ?, 'reply', ?, ?, ?, 'explicit', ?, ?, ?, ?)`,
		m.channel, m.principal, cmd.Outcome, cmd.To, exchange, cmd.Body, ms(now), level, threadRoot)
	if err != nil {
		return core.Result{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return core.Result{}, err
	}
	if err := deliver(ctx, tx, id, target{principal: asker, alias: askerAlias}, false, now); err != nil {
		return core.Result{}, err
	}

	if unread {
		if err := foldRound(ctx, tx, m.principal, cmd.To, threadRoot, level); err != nil {
			return core.Result{}, err
		}
	}
	out := ReplyResult{ID: id, Channel: m.channelName, Ask: cmd.To, Outcome: cmd.Outcome, To: askerAlias, DueAtMS: due, Obligation: "open"}
	if cmd.Outcome == OutcomeMoreTime {
		// The obligation stays open with its original deadline; nothing is settled.
		if _, err := tx.ExecContext(ctx,
			`UPDATE obligation SET more_time_at_ms = ? WHERE message_id = ? AND principal_id = ?`,
			ms(now), cmd.To, m.principal); err != nil {
			return core.Result{}, err
		}
	} else {
		out.OriginalUnread = unread
		out.Obligation = "replied"
		if cmd.Outcome == OutcomeDecline {
			out.Obligation = "declined"
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE obligation SET state = ?, settled_at_ms = ?, settled_by_id = ?, settlement_message_id = ?
			 WHERE message_id = ? AND principal_id = ? AND state = 'open'`,
			out.Obligation, ms(now), m.principal, id, cmd.To, m.principal); err != nil {
			return core.Result{}, err
		}
		if err := receipt(ctx, tx, cmd.To, m.principal, "settled", "cli", now); err != nil {
			return core.Result{}, err
		}
		if late := ms(now) - due; late > 0 {
			out.LateByMS = late
		}
	}
	if err := observe(ctx, tx, m.principal, now); err != nil {
		return core.Result{}, err
	}
	if out.Delivery, err = replyDelivery(ctx, tx, id, &s.waits, now); err != nil {
		return core.Result{}, err
	}
	r, err := OK(out)
	if err != nil {
		return core.Result{}, err
	}
	return r, record(ctx, tx, m.principal, req.OpKey, scope, cmd, r, now)
}

func replyDelivery(ctx context.Context, tx store.Tx, id int64, waits *waitPresence, now time.Time) (Delivery, error) {
	d, err := deliveries(ctx, tx, id, waits, now)
	if err != nil {
		return Delivery{}, err
	}
	if len(d) != 1 {
		return Delivery{}, fmt.Errorf("domain: reply %d has %d recipients", id, len(d))
	}
	return d[0], nil
}

// Refresh both sides on replay: the asker may have read the reply and the
// recipient may have read the original request since the first response.
func withCurrentReplyDelivery(ctx context.Context, tx store.Tx, r core.Result, principal int64, waits *waitPresence, now time.Time) (core.Result, error) {
	var out ReplyResult
	if err := json.Unmarshal(r.Data, &out); err != nil {
		return core.Result{}, err
	}
	d, err := replyDelivery(ctx, tx, out.ID, waits, now)
	if err != nil {
		return core.Result{}, err
	}
	out.Delivery = d
	if out.Outcome != OutcomeMoreTime {
		err := tx.QueryRowContext(ctx, `SELECT picked_up_at_ms IS NULL FROM recipient
			WHERE message_id = ? AND principal_id = ?`, out.Ask, principal).Scan(&out.OriginalUnread)
		if err != nil {
			return core.Result{}, err
		}
	}
	return OK(out)
}
