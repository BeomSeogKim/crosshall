package domain

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

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

// waitSnapshot also records in waits that the caller's wait keeps running
// when the snapshot holds nothing that ends it.
func waitSnapshot(ctx context.Context, tx store.Tx, caller core.Caller, cmd Wait, waits *waitPresence, now time.Time) (core.Result, error) {
	result, binding, err := waitSnapshotOf(ctx, tx, caller, cmd, now)
	if err == nil && binding != 0 {
		waits.saw(binding, cmd.WorkOnly, now)
	}
	return result, err
}

// waitSnapshotOf returns the snapshot and, when the wait goes on polling
// after it, the caller's binding.
func waitSnapshotOf(ctx context.Context, tx store.Tx, caller core.Caller, cmd Wait, now time.Time) (core.Result, int64, error) {
	if caller.Role != core.RoleMember {
		return notEnrolled(), 0, nil
	}
	member, ok, err := lookupLedgerMember(ctx, tx, caller)
	if err != nil {
		return core.Result{}, 0, err
	}
	if !ok {
		return notEnrolled(), 0, nil
	}
	summary, err := memberInboxSummary(ctx, tx, member.principal, member.channel)
	if err != nil {
		return core.Result{}, 0, err
	}
	if cmd.Any && cmd.WorkOnly {
		return noPoll(waitFailure(core.ExitUsage, "", "--any and --work-only cannot be used together", "crosshall wait --any, or crosshall wait --work-only", &summary))
	}
	if cmd.Cursor != nil && cmd.Cursor.BindingID != member.binding {
		return noPoll(waitFailure(core.ExitRefused, core.CodeDeparted, "membership changed while waiting", "run crosshall wait again", &summary))
	}
	if cmd.Cursor != nil && cmd.Cursor.MessageID < 0 {
		return noPoll(waitFailure(core.ExitUsage, "", "invalid wait cursor", "run crosshall wait again", &summary))
	}

	// Delivers lets a timeout say that this session needs no wait.
	delivers, err := publishDelivers(ctx, tx, member.binding, now)
	if err != nil {
		return core.Result{}, 0, err
	}
	out := WaitSnapshot{
		Cursor:             WaitCursor{BindingID: member.binding},
		InboxResult:        InboxResult{Unread: []InboxItem{}, Owed: []InboxItem{}, Delivers: delivers},
		MemberInboxSummary: &summary,
	}
	// Message IDs increase across channels. Use the indexed high-water mark
	// so empty polls do not repeatedly scan other channels' messages.
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM message`).Scan(&out.Cursor.MessageID); err != nil {
		return core.Result{}, 0, err
	}
	if cmd.Cursor != nil && cmd.Cursor.MessageID > out.Cursor.MessageID {
		return noPoll(WaitCursorInvalidated(&summary))
	}
	out.CursorFingerprint, err = waitMessageFingerprint(ctx, tx, out.Cursor.MessageID)
	if err != nil {
		return core.Result{}, 0, err
	}
	if cmd.Cursor == nil {
		if cmd.Existing {
			out.InboxResult, err = inboxItems(ctx, tx, member, cmd.WorkOnly)
			if err != nil {
				return core.Result{}, 0, err
			}
			out.Delivers = delivers
			out.Cancelled = nil // a cancellation is not work: it never ends or rides on a wait
		}
		return out.polled(member.binding)
	}
	if cmd.Cursor.MessageID == out.Cursor.MessageID {
		out.PriorCursorFingerprint = out.CursorFingerprint
		return out.polled(member.binding)
	}
	out.PriorCursorFingerprint, err = waitMessageFingerprint(ctx, tx, cmd.Cursor.MessageID)
	if err != nil {
		return core.Result{}, 0, err
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
		return core.Result{}, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var item InboxItem
		if err := rows.Scan(&item.ID, &item.Kind, &item.Sender, &item.DueAtMS); err != nil {
			return core.Result{}, 0, err
		}
		out.Unread = append(out.Unread, item)
	}
	if err := rows.Err(); err != nil {
		return core.Result{}, 0, err
	}
	if cmd.Existing && len(out.Unread) != 0 {
		inbox, err := inboxItems(ctx, tx, member, cmd.WorkOnly)
		if err != nil {
			return core.Result{}, 0, err
		}
		out.Owed = inbox.Owed
	}
	return out.polled(member.binding)
}

// polled returns the snapshot with the binding whose wait goes on polling,
// which it does unless something unread ends it.
func (out WaitSnapshot) polled(binding int64) (core.Result, int64, error) {
	r, err := OK(out)
	if len(out.Unread) != 0 {
		binding = 0
	}
	return r, binding, err
}

func noPoll(r core.Result, err error) (core.Result, int64, error) { return r, 0, err }

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

// HallDelivers is what a member the hall delivers to is told where a
// member that has to poll is told to wait: an empty inbox and a wait
// timeout with nothing unread.
const HallDelivers = "The hall brings new work to this session: no wait is needed."

// WaitTimeout adds the latest successful hall snapshot to a local timeout.
// The snapshot is an inbox check, so the next step is the pickup when
// something is unread, another wait for a member that has to poll, and
// none for one the hall delivers to, which Delivers marks.
func WaitTimeout(summary *MemberInboxSummary, delivers bool) (core.Result, error) {
	next := "crosshall wait --existing --work-only --timeout 9m"
	switch {
	case summary != nil && len(summary.Pending) > 0:
		next, delivers = "crosshall read", false
	case delivers:
		next = ""
	}
	result, err := OK(struct {
		Refusal
		*MemberInboxSummary
		Delivers bool `json:"delivers,omitempty"`
	}{Refusal{Message: "no new messages before the wait timeout", Hint: next}, summary, delivers})
	result.Status, result.Code = core.ExitWaitTimeout, core.CodeWaitTimeout
	return result, err
}

func waitFailure(status core.ExitCode, code core.Code, message, hint string, summary *MemberInboxSummary) (core.Result, error) {
	result, err := OK(struct {
		Refusal
		*MemberInboxSummary
	}{Refusal: Refusal{Message: message, Hint: hint}, MemberInboxSummary: summary})
	result.Status, result.Code = status, code
	return result, err
}
