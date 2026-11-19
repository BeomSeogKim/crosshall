package domain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

const VerbRead = "read"

// Read retrieves selected messages or the next page of unread messages.
type Read struct {
	IDs []int64 `json:"ids,omitempty"`
}

func (Read) command()     {}
func (Read) Verb() string { return VerbRead }

// ReadResult is a page of message bodies and their metadata.
type ReadResult struct {
	Messages []ReadMessage `json:"messages"`
}

// ReadMessage describes the stored message and its thread at read time.
type ReadMessage struct {
	ID              int64         `json:"id"`
	Kind            string        `json:"kind"`
	Channel         string        `json:"channel"`
	Sender          string        `json:"sender"`
	SenderID        int64         `json:"sender_id"`
	Body            string        `json:"body"`
	DueAtMS         *int64        `json:"due_at_ms,omitempty"`
	InReplyTo       *int64        `json:"in_reply_to,omitempty"`
	Exchange        *int64        `json:"exchange_id,omitempty"`
	Outcome         string        `json:"outcome,omitempty"`
	ObligationState string        `json:"obligation_state,omitempty"`
	ReplyIDs        []int64       `json:"reply_ids,omitempty"`
	Replies         []ReadMessage `json:"replies,omitempty"`
}

type readItem struct {
	ID          int64   `json:"id"`
	ReplyIDs    []int64 `json:"reply_ids,omitempty"`
	ShowReplies bool    `json:"show_replies,omitempty"`
}

type readSnapshot struct {
	Items []readItem `json:"items"`
}

type memberView struct {
	binding, principal, channel int64
	name                        string
	cursor                      uint64
	hintAcked                   bool
}

func lookupLedgerMember(ctx context.Context, tx store.Tx, caller core.Caller) (memberView, bool, error) {
	var m memberView
	if caller.PrincipalID == nil {
		return m, false, nil
	}
	m.principal = *caller.PrincipalID
	// A newly rejoined member can be looked up using its existing principal.
	err := tx.QueryRowContext(ctx, `SELECT b.id, b.channel_id, c.name, b.roster_ack_seq, b.hint_acked
		FROM binding b JOIN channel c ON c.id = b.channel_id
		WHERE b.principal_id = ? AND b.state = 'live'`, m.principal).
		Scan(&m.binding, &m.channel, &m.name, &m.cursor, &m.hintAcked)
	if errors.Is(err, sql.ErrNoRows) {
		return m, false, nil
	}
	if err == nil && caller.BindingID != nil && *caller.BindingID != m.binding {
		return m, false, nil
	}
	return m, err == nil, err
}

func (s *Service) read(ctx context.Context, tx store.Tx, caller core.Caller, req Request, cmd Read, now time.Time) (core.Result, error) {
	if caller.Role != core.RoleMember {
		return notEnrolled(), nil
	}
	member, ok, err := lookupLedgerMember(ctx, tx, caller)
	if err != nil {
		return core.Result{}, err
	}
	if !ok {
		return notEnrolled(), nil
	}
	unique := make(map[int64]bool, len(cmd.IDs))
	ids := make([]int64, 0, len(cmd.IDs))
	for _, id := range cmd.IDs {
		if id <= 0 {
			return Fail(core.ExitUsage, "", "message IDs must be positive integers", ""), nil
		}
		if !unique[id] {
			ids = append(ids, id)
			unique[id] = true
		}
	}
	cmd.IDs = ids
	scope := "channel:" + member.name
	if prior, found, err := replay(ctx, tx, member.principal, req.OpKey, scope, cmd); err != nil || found {
		if err != nil || prior.Status != core.ExitOK {
			return prior, err
		}
		var snapshot readSnapshot
		if err := json.Unmarshal(prior.Data, &snapshot); err != nil {
			return core.Result{}, err
		}
		out, err := hydrateRead(ctx, tx, member, snapshot)
		if err != nil {
			return core.Result{}, err
		}
		return OK(out)
	}
	if len(ids) == 0 {
		ids, err = selectIDs(ctx, tx, `SELECT m.id FROM message m JOIN recipient r ON r.message_id = m.id
			WHERE m.channel_id = ? AND r.principal_id = ? AND r.picked_up_at_ms IS NULL ORDER BY m.id LIMIT 10`,
			member.channel, member.principal)
		if err != nil {
			return core.Result{}, err
		}
	}

	snapshot := readSnapshot{Items: make([]readItem, 0, len(ids))}
	out := ReadResult{Messages: make([]ReadMessage, 0, len(ids))}
	for _, id := range ids {
		msg, err := readBody(ctx, tx, member, id)
		if errors.Is(err, sql.ErrNoRows) {
			return Refuse(core.CodeNotFound, fmt.Sprintf("message #%d was not found", id), "crosshall inbox"), nil
		}
		if err != nil {
			return core.Result{}, err
		}
		item := readItem{ID: id}
		if msg.Kind == "ask" {
			item.ReplyIDs, err = selectIDs(ctx, tx, `SELECT id FROM message WHERE channel_id = ?
				AND kind = 'reply' AND in_reply_to = ? ORDER BY id`, member.channel, id)
			if err != nil {
				return core.Result{}, err
			}
			item.ShowReplies = member.principal == msg.SenderID
			if err := loadReplies(ctx, tx, member, item, &msg); err != nil {
				return core.Result{}, err
			}
		}
		snapshot.Items = append(snapshot.Items, item)
		out.Messages = append(out.Messages, msg)
	}
	// Check every requested ID before recording a pickup. A refusal commits
	// none of the preceding requested messages.
	for _, msg := range out.Messages {
		if err := recordPickup(ctx, tx, member, msg, now); err != nil {
			return core.Result{}, err
		}
		for _, reply := range msg.Replies {
			if err := recordPickup(ctx, tx, member, reply, now); err != nil {
				return core.Result{}, err
			}
		}
	}
	if err := observe(ctx, tx, member.principal, now); err != nil {
		return core.Result{}, err
	}
	saved, err := OK(snapshot)
	if err != nil {
		return core.Result{}, err
	}
	if err := record(ctx, tx, member.principal, req.OpKey, scope, cmd, saved, now); err != nil {
		return core.Result{}, err
	}
	return OK(out)
}

func hydrateRead(ctx context.Context, tx store.Tx, member memberView, snapshot readSnapshot) (ReadResult, error) {
	out := ReadResult{Messages: make([]ReadMessage, 0, len(snapshot.Items))}
	for _, item := range snapshot.Items {
		msg, err := readBody(ctx, tx, member, item.ID)
		if err != nil {
			return ReadResult{}, err
		}
		if err := loadReplies(ctx, tx, member, item, &msg); err != nil {
			return ReadResult{}, err
		}
		out.Messages = append(out.Messages, msg)
	}
	return out, nil
}

func loadReplies(ctx context.Context, tx store.Tx, member memberView, item readItem, msg *ReadMessage) error {
	if !item.ShowReplies {
		msg.ReplyIDs = item.ReplyIDs
		return nil
	}
	for _, id := range item.ReplyIDs {
		reply, err := readBody(ctx, tx, member, id)
		if err != nil {
			return err
		}
		msg.Replies = append(msg.Replies, reply)
	}
	return nil
}

func readBody(ctx context.Context, tx store.Tx, member memberView, id int64) (ReadMessage, error) {
	msg := ReadMessage{ID: id, Channel: member.name}
	err := tx.QueryRowContext(ctx, `SELECT m.kind, m.sender_id, COALESCE(b.alias, ''), m.body,
		m.due_at_ms, m.in_reply_to, m.exchange_id, COALESCE(m.outcome, ''), COALESCE(o.state, '')
		FROM message m LEFT JOIN binding b ON b.channel_id = m.channel_id AND b.principal_id = m.sender_id
		LEFT JOIN obligation o ON o.message_id = m.id AND o.principal_id = ?
		WHERE m.channel_id = ? AND m.id = ?`, member.principal, member.channel, id).
		Scan(&msg.Kind, &msg.SenderID, &msg.Sender, &msg.Body, &msg.DueAtMS, &msg.InReplyTo, &msg.Exchange, &msg.Outcome, &msg.ObligationState)
	return msg, err
}

func recordPickup(ctx context.Context, tx store.Tx, member memberView, msg ReadMessage, now time.Time) error {
	update, err := tx.ExecContext(ctx, `UPDATE recipient SET picked_up_at_ms = ?
		WHERE message_id = ? AND principal_id = ? AND picked_up_at_ms IS NULL`, ms(now), msg.ID, member.principal)
	if err != nil {
		return err
	}
	n, err := update.RowsAffected()
	if err != nil || n == 0 {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO receipt(message_id, principal_id, stage, seq, at_ms, evidence)
		VALUES (?, ?, 'picked_up', 1, ?, 'cli')`, msg.ID, member.principal, ms(now)); err != nil {
		return err
	}
	if msg.Exchange != nil {
		_, err = tx.ExecContext(ctx, "UPDATE binding SET context_exchange_id = ? WHERE id = ?", *msg.Exchange, member.binding)
	}
	return err
}

func selectIDs(ctx context.Context, tx store.Tx, query string, args ...any) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
