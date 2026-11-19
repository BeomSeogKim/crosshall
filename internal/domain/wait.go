package domain

import (
	"context"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

const VerbWait = "wait"

// Wait requests one read-only snapshot. The first request establishes the
// starting cursor without returning messages.
type Wait struct {
	Any    bool        `json:"any,omitempty"`
	Cursor *WaitCursor `json:"cursor,omitempty"`
}

func (Wait) command()     {}
func (Wait) Verb() string { return VerbWait }

type WaitCursor struct {
	BindingID int64 `json:"binding_id"`
	MessageID int64 `json:"message_id"`
}

type WaitSnapshot struct {
	Cursor WaitCursor `json:"cursor"`
	InboxResult
}

func waitSnapshot(ctx context.Context, tx store.Tx, caller core.Caller, cmd Wait) (core.Result, error) {
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
	if cmd.Cursor != nil {
		if cmd.Cursor.BindingID != member.binding {
			return Refuse(core.CodeDeparted, "membership changed while waiting", "run crosshall wait again"), nil
		}
		if cmd.Cursor.MessageID < 0 {
			return Fail(core.ExitUsage, "", "invalid wait cursor", ""), nil
		}
	}

	out := WaitSnapshot{
		Cursor:      WaitCursor{BindingID: member.binding},
		InboxResult: InboxResult{Unread: []InboxItem{}, Owed: []InboxItem{}},
	}
	// Message IDs increase across channels. Use the indexed high-water mark
	// so empty polls do not repeatedly scan other channels' messages.
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM message`).Scan(&out.Cursor.MessageID); err != nil {
		return core.Result{}, err
	}
	if cmd.Cursor == nil || cmd.Cursor.MessageID >= out.Cursor.MessageID {
		return OK(out)
	}

	rows, err := tx.QueryContext(ctx, `SELECT m.id, m.kind, COALESCE(b.alias, ''), m.due_at_ms
		FROM message m
		LEFT JOIN binding b ON b.channel_id = m.channel_id AND b.principal_id = m.sender_id
		WHERE m.id > ? AND m.channel_id = ? AND m.sender_id <> ?
			AND (? OR EXISTS (
				SELECT 1 FROM recipient r WHERE r.message_id = m.id AND r.principal_id = ?
			))
		ORDER BY m.id`, cmd.Cursor.MessageID, member.channel, member.principal, cmd.Any, member.principal)
	if err != nil {
		return core.Result{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var item InboxItem
		if err := rows.Scan(&item.ID, &item.Kind, &item.Sender, &item.DueAtMS); err != nil {
			return core.Result{}, err
		}
		out.Unread = append(out.Unread, item)
	}
	if err := rows.Err(); err != nil {
		return core.Result{}, err
	}
	return OK(out)
}
