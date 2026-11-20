package domain

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

const (
	VerbWait                            = "wait"
	CodeWaitCursorInvalidated core.Code = "wait_cursor_invalidated"
)

// Wait requests one read-only snapshot. The first request establishes the
// starting cursor and optionally returns inbox work already present.
type Wait struct {
	Any      bool        `json:"any,omitempty"`
	Existing bool        `json:"existing,omitempty"`
	WorkOnly bool        `json:"work_only,omitempty"`
	Cursor   *WaitCursor `json:"cursor,omitempty"`
}

func (Wait) command()     {}
func (Wait) Verb() string { return VerbWait }

type WaitCursor struct {
	BindingID int64 `json:"binding_id"`
	MessageID int64 `json:"message_id"`
}

type WaitSnapshot struct {
	Cursor WaitCursor `json:"cursor"`
	// Older dispatchers strictly decode wait requests, so fingerprints stay in replies.
	CursorFingerprint      string `json:"cursor_fingerprint,omitempty"`
	PriorCursorFingerprint string `json:"prior_cursor_fingerprint,omitempty"`
	InboxResult
	*MemberInboxSummary
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
	summary, err := memberInboxSummary(ctx, tx, member.principal, member.channel)
	if err != nil {
		return core.Result{}, err
	}
	if cmd.Any && cmd.WorkOnly {
		return waitFailure(core.ExitUsage, "", "--any and --work-only cannot be used together", "crosshall wait --any, or crosshall wait --work-only", &summary)
	}
	if cmd.Cursor != nil && cmd.Cursor.BindingID != member.binding {
		return waitFailure(core.ExitRefused, core.CodeDeparted, "membership changed while waiting", "run crosshall wait again", &summary)
	}
	if cmd.Cursor != nil && cmd.Cursor.MessageID < 0 {
		return waitFailure(core.ExitUsage, "", "invalid wait cursor", "run crosshall wait again", &summary)
	}

	out := WaitSnapshot{
		Cursor:             WaitCursor{BindingID: member.binding},
		InboxResult:        InboxResult{Unread: []InboxItem{}, Owed: []InboxItem{}},
		MemberInboxSummary: &summary,
	}
	// Message IDs increase across channels. Use the indexed high-water mark
	// so empty polls do not repeatedly scan other channels' messages.
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM message`).Scan(&out.Cursor.MessageID); err != nil {
		return core.Result{}, err
	}
	if cmd.Cursor != nil && cmd.Cursor.MessageID > out.Cursor.MessageID {
		return WaitCursorInvalidated(&summary)
	}
	out.CursorFingerprint, err = waitMessageFingerprint(ctx, tx, out.Cursor.MessageID)
	if err != nil {
		return core.Result{}, err
	}
	if cmd.Cursor == nil {
		if cmd.Existing {
			out.InboxResult, err = inboxItems(ctx, tx, member, cmd.WorkOnly)
			if err != nil {
				return core.Result{}, err
			}
			out.Cancelled = nil // a cancellation is not work: it never ends or rides on a wait
		}
		return OK(out)
	}
	if cmd.Cursor.MessageID == out.Cursor.MessageID {
		out.PriorCursorFingerprint = out.CursorFingerprint
		return OK(out)
	}
	out.PriorCursorFingerprint, err = waitMessageFingerprint(ctx, tx, cmd.Cursor.MessageID)
	if err != nil {
		return core.Result{}, err
	}

	rows, err := tx.QueryContext(ctx, `SELECT m.id, m.kind, COALESCE(b.alias, ''), m.due_at_ms
		FROM message m
		LEFT JOIN binding b ON b.channel_id = m.channel_id AND b.principal_id = m.sender_id
		WHERE m.id > ? AND m.channel_id = ? AND m.sender_id <> ? AND `+notMoreTime+`
			AND (NOT ? OR m.kind <> 'note')
			AND (? OR EXISTS (
				SELECT 1 FROM recipient r WHERE r.message_id = m.id AND r.principal_id = ?
			))
			AND NOT EXISTS (SELECT 1 FROM obligation oc
				WHERE oc.message_id = m.id AND oc.principal_id = ? AND oc.state <> 'open')
		ORDER BY m.id`, cmd.Cursor.MessageID, member.channel, member.principal, cmd.WorkOnly, cmd.Any, member.principal, member.principal)
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
	if cmd.Existing && len(out.Unread) != 0 {
		inbox, err := inboxItems(ctx, tx, member, cmd.WorkOnly)
		if err != nil {
			return core.Result{}, err
		}
		out.Owed = inbox.Owed
	}
	return OK(out)
}

func waitMessageFingerprint(ctx context.Context, tx store.Tx, id int64) (string, error) {
	if id == 0 {
		return "", nil
	}
	var message string
	err := tx.QueryRowContext(ctx, `SELECT json_array(id, channel_id, sender_id, kind, created_at_ms)
		FROM message WHERE id = ?`, id).Scan(&message)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256([]byte(message))
	return hex.EncodeToString(digest[:]), nil
}

func WaitCursorInvalidated(summary *MemberInboxSummary) (core.Result, error) {
	return waitFailure(core.ExitRefused, CodeWaitCursorInvalidated, "message history changed while waiting", "crosshall inbox", summary)
}

// WaitTimeout adds the latest successful hall snapshot to a local timeout.
// The snapshot is an inbox check, so the next step is the pickup when
// something is unread, another wait for a member that has to poll, and
// nothing for one the hall delivers to.
func WaitTimeout(summary *MemberInboxSummary, delivers bool) (core.Result, error) {
	next := "crosshall wait --existing --work-only --timeout 9m"
	switch {
	case summary != nil && len(summary.Pending) > 0:
		next = "crosshall read"
	case delivers:
		next = ""
	}
	return waitFailure(core.ExitWaitTimeout, core.CodeWaitTimeout, "no new messages before the wait timeout", next, summary)
}

func waitFailure(status core.ExitCode, code core.Code, message, hint string, summary *MemberInboxSummary) (core.Result, error) {
	result, err := OK(struct {
		Refusal
		*MemberInboxSummary
	}{Refusal: Refusal{Message: message, Hint: hint}, MemberInboxSummary: summary})
	result.Status, result.Code = status, code
	return result, err
}
