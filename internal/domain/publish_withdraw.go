package domain

import (
	"context"
	"strings"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

// Reasons a pending notice is withdrawn before it runs.
const (
	// WithdrawSessionEnded: its route ended. A notice still queued would
	// run when the thread is next loaded, as a turn about old work; the
	// member's next prompt shows what is still unread.
	WithdrawSessionEnded = "session ended"
	// WithdrawNothingToRead: every item was read, offered in a prompt, or
	// is gone. Its turn would find nothing to do.
	WithdrawNothingToRead = "nothing left to read"
)

// WithdrawCandidate is an open, submitted notice on a thread lane that
// should not run: its target without the home, which the caller supplies.
type WithdrawCandidate struct {
	ID     int64
	Target PublishTarget
	// LaneHome is the home its lane names; a transport may withdraw only
	// through that home.
	LaneHome string
	Reason   string
}

// withdrawCandidatesQuery reads only open notices, through the partial
// index publish_attempt_binding: the publisher runs it in every tick of the
// writer. No ORDER BY, which would scan the whole table.
var withdrawCandidatesQuery = `SELECT a.id, a.adapter, a.lane, a.reserved_at_ms, a.process_pid, a.process_start_ms,
		NOT EXISTS (SELECT 1 FROM publish_route r WHERE r.binding_id = a.binding_id AND r.state = 'active'
			AND r.thread_id IS NOT NULL AND r.vendor_home IS NOT NULL AND ` + store.PublishThreadLane() + ` = a.lane)
		FROM publish_attempt a
		WHERE ` + openPublishLane + ` AND a.state IN ('submitted', 'uncertain') AND a.lane LIKE 'thread:%'`

// stillDueItem is an item of attempt i the notice still carries: for a
// notice, unread work (pendingPublishItem); for a resume notice, whose
// items were read or told of before and which announces none of them, a
// request the member still owes (openResumeItem).
const stillDueItem = `CASE WHEN EXISTS (SELECT 1 FROM announcement n WHERE n.principal_id = i.principal_id
		AND n.message_id = i.message_id AND n.via = 'publish' AND n.ref = i.attempt_id)
	THEN ` + pendingPublishItem + ` ELSE ` + openResumeItem + ` END`

// WithdrawCandidates lists the submitted or uncertain notices on thread
// lanes whose route has ended or whose items are no longer pending. A
// process lane is never listed: its queue ends with its process (Retired).
func WithdrawCandidates(ctx context.Context, tx store.Tx) ([]WithdrawCandidate, error) {
	rows, err := tx.QueryContext(ctx, withdrawCandidatesQuery)
	if err != nil {
		return nil, err
	}
	var out []WithdrawCandidate
	for rows.Next() {
		var c WithdrawCandidate
		var lane string
		var reservedAt int64
		var ended bool
		if err := rows.Scan(&c.ID, &c.Target.Adapter, &lane, &reservedAt, &c.Target.Queue.PID, &c.Target.Queue.StartMS, &ended); err != nil {
			rows.Close()
			return nil, err
		}
		// thread:<adapter>:<thread>:<home>; the home may hold colons.
		parts := strings.SplitN(lane, ":", 4)
		if len(parts) != 4 || parts[1] != c.Target.Adapter || parts[2] == "" || parts[3] == "" {
			continue
		}
		c.Target.Address, c.LaneHome = parts[2], parts[3]
		c.Target.Attempt = AttemptIdentity(c.ID, reservedAt)
		if ended {
			c.Reason = WithdrawSessionEnded
		}
		out = append(out, c)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	kept := out[:0]
	for _, c := range out {
		if c.Reason == "" {
			due, err := carriesDueItem(ctx, tx, c.ID)
			if err != nil {
				return nil, err
			}
			if due {
				continue
			}
			c.Reason = WithdrawNothingToRead
		}
		kept = append(kept, c)
	}
	return kept, nil
}

// WithdrawPublishAttempt settles an attempt the transport proved it
// withdrew: the notice can no longer run. Its lane frees and no pickup is
// claimed; the items stay as they are.
func WithdrawPublishAttempt(ctx context.Context, tx store.Tx, id int64, reason string, now time.Time) error {
	return settlePublishAttempt(ctx, tx, id, "withdrawn: "+reason, now)
}

// carriesDueItem holds while attempt id still carries an item that is due.
func carriesDueItem(ctx context.Context, tx store.Tx, id int64) (bool, error) {
	pending, err := selectIDs(ctx, tx, `SELECT x.message_id FROM publish_attempt_item i
		JOIN recipient x ON x.principal_id = i.principal_id AND x.message_id = i.message_id
		JOIN message m ON m.id = x.message_id
		WHERE i.attempt_id = ? AND `+stillDueItem, id)
	return len(pending) > 0, err
}

// SettleNotPendingAttempt settles an attempt the transport reports not
// pending, when nothing it carries is still due: it ran, or will run to
// find nothing, so it can no longer do anything and need not hold its lane
// (#446). A late receipt for it is judged stale. It reports whether it
// settled; a notice still carrying due work stays held for the owner.
func SettleNotPendingAttempt(ctx context.Context, tx store.Tx, id int64, now time.Time) (bool, error) {
	due, err := carriesDueItem(ctx, tx, id)
	if err != nil || due {
		return false, err
	}
	return true, settlePublishAttempt(ctx, tx, id, "not pending: "+WithdrawNothingToRead, now)
}
