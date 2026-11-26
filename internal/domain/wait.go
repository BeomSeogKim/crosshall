package domain

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
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
	BindingID  int64 `json:"binding_id"`
	MessageID  int64 `json:"message_id"`
	Generation int64 `json:"membership_generation,omitempty"`
}

type WaitSnapshot struct {
	Cursor WaitCursor `json:"cursor"`
	// Older dispatchers strictly decode wait requests, so fingerprints stay in replies.
	CursorFingerprint      string `json:"cursor_fingerprint,omitempty"`
	PriorCursorFingerprint string `json:"prior_cursor_fingerprint,omitempty"`
	InitialCursorRecovered bool   `json:"initial_cursor_recovered,omitempty"`
	InboxResult
	*MemberInboxSummary
}

func (s *Service) waitSnapshot(ctx context.Context, tx store.Tx, caller core.Caller, cmd Wait, linked bool, key string, now time.Time) (core.Result, error) {
	var attempt waitInvocation
	var baseline *waitBaseline
	if linked && caller.Role == core.RoleMember && caller.PrincipalID != nil && cmd.Cursor == nil {
		if id, retry := wire.WaitAttempt(key); id != "" {
			attempt = waitInvocation{principal: *caller.PrincipalID, id: id}
			if prior, found := s.waits.baseline(attempt); found && prior.mode == cmd {
				baseline = &prior
			} else if found || retry {
				return waitFailure(core.ExitRefused, CodeWaitCursorInvalidated, "the initial wait cursor is unavailable", "crosshall inbox", nil)
			}
		}
	}
	result, poll, err := s.waitSnapshotOf(ctx, tx, caller, cmd, baseline, now)
	switch {
	case err != nil || poll.cursor.BindingID == 0:
	case linked && cmd.Cursor == nil:
		// Do not report presence until the linked client has the cursor.
		s.waits.forget(poll.cursor.BindingID)
		if attempt.id != "" && baseline == nil {
			s.waits.rememberBaseline(attempt, waitBaseline{cursor: poll.cursor, fingerprint: poll.fingerprint, mode: cmd})
		}
	case poll.ends:
		s.waits.forget(poll.cursor.BindingID)
	default:
		s.waits.saw(poll.cursor, cmd.WorkOnly, now)
	}
	return result, err
}

// snapshotPoll names the binding a successful snapshot answered, zero for a
// refusal, and whether the snapshot ends the wait.
type snapshotPoll struct {
	cursor      WaitCursor
	fingerprint string
	ends        bool
}

func (s *Service) waitSnapshotOf(ctx context.Context, tx store.Tx, caller core.Caller, cmd Wait, baseline *waitBaseline, now time.Time) (core.Result, snapshotPoll, error) {
	if caller.Role != core.RoleMember {
		return notEnrolled(), snapshotPoll{}, nil
	}
	member, ok, err := lookupLedgerMember(ctx, tx, caller)
	if err != nil {
		return core.Result{}, snapshotPoll{}, err
	}
	if !ok {
		return notEnrolled(), snapshotPoll{}, nil
	}
	cursor := cmd.Cursor
	if baseline != nil {
		cursor = &baseline.cursor
	}
	if cursor != nil && (cursor.BindingID != member.binding ||
		cursor.Generation != member.generation && (cursor.Generation != 0 || member.generation > 1)) {
		return noPoll(waitFailure(core.ExitRefused, core.CodeDeparted, "membership changed while waiting", "run crosshall wait again", nil))
	}
	summary, err := memberInboxSummary(ctx, tx, member.principal, member.channel)
	if err != nil {
		return core.Result{}, snapshotPoll{}, err
	}
	if cmd.Any && cmd.WorkOnly {
		return noPoll(waitFailure(core.ExitUsage, "", "--any and --work-only cannot be used together", "crosshall wait --any, or crosshall wait --work-only", &summary))
	}
	if cmd.Cursor != nil && cmd.Cursor.MessageID < 0 {
		return noPoll(waitFailure(core.ExitUsage, "", "invalid wait cursor", "run crosshall wait again", &summary))
	}

	// Delivers and AtNextPrompt let a timeout say that this session needs no wait.
	delivers, atPrompt, err := s.standby(ctx, tx, member.binding, now)
	if err != nil {
		return core.Result{}, snapshotPoll{}, err
	}
	out := WaitSnapshot{
		Cursor:             WaitCursor{BindingID: member.binding, Generation: member.generation},
		InboxResult:        InboxResult{Unread: []InboxItem{}, Owed: []InboxItem{}, Delivers: delivers, AtNextPrompt: atPrompt},
		MemberInboxSummary: &summary,
	}
	if baseline != nil {
		out.Cursor = baseline.cursor
		out.InitialCursorRecovered = true
	} else {
		// Use the indexed high-water mark without scanning other channels.
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM message`).Scan(&out.Cursor.MessageID); err != nil {
			return core.Result{}, snapshotPoll{}, err
		}
	}
	if cmd.Cursor != nil && cmd.Cursor.MessageID > out.Cursor.MessageID {
		return noPoll(WaitCursorInvalidated(&summary))
	}
	out.CursorFingerprint, err = waitMessageFingerprint(ctx, tx, out.Cursor.MessageID)
	if err != nil {
		return core.Result{}, snapshotPoll{}, err
	}
	if baseline != nil && out.CursorFingerprint != baseline.fingerprint {
		return noPoll(WaitCursorInvalidated(&summary))
	}
	if cmd.Cursor == nil {
		if cmd.Existing {
			out.InboxResult, err = inboxItems(ctx, tx, member, cmd.WorkOnly)
			if err != nil {
				return core.Result{}, snapshotPoll{}, err
			}
			out.Delivers, out.AtNextPrompt = delivers, atPrompt
			out.Cancelled = nil // a cancellation is not work: it never ends or rides on a wait
		}
		return out.polled()
	}
	if cmd.Cursor.MessageID == out.Cursor.MessageID {
		out.PriorCursorFingerprint = out.CursorFingerprint
		return out.polled()
	}
	out.PriorCursorFingerprint, err = waitMessageFingerprint(ctx, tx, cmd.Cursor.MessageID)
	if err != nil {
		return core.Result{}, snapshotPoll{}, err
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
		return core.Result{}, snapshotPoll{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var item InboxItem
		if err := rows.Scan(&item.ID, &item.Kind, &item.Sender, &item.DueAtMS); err != nil {
			return core.Result{}, snapshotPoll{}, err
		}
		out.Unread = append(out.Unread, item)
	}
	if err := rows.Err(); err != nil {
		return core.Result{}, snapshotPoll{}, err
	}
	if cmd.Existing && len(out.Unread) != 0 {
		inbox, err := inboxItems(ctx, tx, member, cmd.WorkOnly)
		if err != nil {
			return core.Result{}, snapshotPoll{}, err
		}
		out.Owed = inbox.Owed
	}
	return out.polled()
}

// polled returns the snapshot with its binding. Something unread ends the
// wait.
func (out WaitSnapshot) polled() (core.Result, snapshotPoll, error) {
	r, err := OK(out)
	return r, snapshotPoll{cursor: out.Cursor, fingerprint: out.CursorFingerprint, ends: len(out.Unread) != 0}, err
}

func noPoll(r core.Result, err error) (core.Result, snapshotPoll, error) {
	return r, snapshotPoll{}, err
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

// HallDelivers is what a member the hall delivers to is told where a
// member that has to poll is told to wait: an empty inbox and a wait
// timeout with nothing unread.
const HallDelivers = "The hall brings new work to this session: no wait is needed."

// AtNextPrompt is what a member whose agent does not start a turn when a
// background command exits is told instead of a wait: holding a turn on a
// wait costs a model request at each poll.
const AtNextPrompt = "Do not wait: new work shows at your next prompt. End the turn when you have nothing else to do."

// AtNextPromptJoin tells such a member its rule 4 case where it reads the
// rules, so its first turn does not run a wait only to learn it.
const AtNextPromptJoin = "[crosshall] In this session new work shows at your next prompt: under rule 4, do not run `wait`; end the turn when you have nothing else to do."

// standby says how an idle member stands by: the hall delivers new work to
// it (delivers), or its next prompt shows new work and its agent does not
// start a turn when a background command exits, so it ends its turn
// instead of waiting (atPrompt). Neither means it waits.
func (s *Service) standby(ctx context.Context, tx store.Tx, binding int64, now time.Time) (delivers, atPrompt bool, err error) {
	if delivers, err = publishDelivers(ctx, tx, binding, now); err != nil || delivers {
		return delivers, false, err
	}
	adapter, err := s.promptLineAdapter(ctx, tx, binding)
	if err != nil || adapter == "" {
		return false, false, err
	}
	a, ok := publish.Lookup(adapter)
	return false, ok && !a.BackgroundWakes(), nil
}

// WaitTimeout adds the latest successful hall snapshot to a local timeout.
// The snapshot is an inbox check, so the next step is the pickup when
// something is unread, another wait for a member that has to poll, none
// for one the hall delivers to, which Delivers marks, and the end of the
// turn for one whose agent cannot stand by on a background wait.
func WaitTimeout(summary *MemberInboxSummary, delivers, atPrompt bool) (core.Result, error) {
	next := "crosshall wait --existing --work-only --timeout 9m"
	switch {
	case summary != nil && len(summary.Pending) > 0:
		next, delivers = "crosshall read", false
	case delivers:
		next = ""
	case atPrompt:
		next = AtNextPrompt
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
