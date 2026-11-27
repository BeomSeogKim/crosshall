package domain

import (
	"context"
	"strconv"
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

// SettledByLaterPrompt is why a process-lane notice is settled once its
// launch opened a later turn with no receipt for it and nothing it carries
// is due (#450).
const SettledByLaterPrompt = "a later prompt carried no receipt: " + WithdrawNothingToRead

// laterPromptNotices are the open notices on process lanes that were
// submitted, and whose launch opened a turn of its own after the
// submission completed (resolved_at_ms, taken once the transport
// returned). The notice's own prompt observes it in the transaction that
// opens its turn, so such a turn came from a prompt that carried no
// receipt for it. An uncertain send has no completed submission and stays
// held. A typed-ahead placeholder is input into a running turn, which
// Claude Code may absorb, not a turn of its own. It reads only open
// notices, through the partial index publish_attempt_binding.
var laterPromptNotices = `SELECT a.id FROM publish_attempt a
	WHERE ` + openPublishLane + ` AND a.state = 'submitted' AND a.lane LIKE 'process:%'
		AND EXISTS (SELECT 1 FROM turn t WHERE t.principal_id = a.principal_id AND t.launch_ref = a.launch_ref
			AND t.opened_hook_at_ms > a.resolved_at_ms
			AND substr(t.turn_ref, 1, ` + strconv.Itoa(len(typedAheadPrefix)) + `) <> '` + typedAheadPrefix + `')`

// SettleLaterPromptNotices settles the process-lane notices a later prompt
// of their launch passed by while nothing they carry is due (#450). A
// process lane has no withdrawal (WithdrawCandidates lists none), so such
// a notice held its member until a receipt, the end of its process, or the
// owner's resume. If it still runs, its turn finds nothing to do, and its
// late receipt is judged stale. A notice that still carries due work stays
// held, and the publish_held owner alert still fires for it.
func SettleLaterPromptNotices(ctx context.Context, tx store.Tx, now time.Time) error {
	ids, err := selectIDs(ctx, tx, laterPromptNotices)
	if err != nil {
		return err
	}
	for _, id := range ids {
		due, err := carriesDueItem(ctx, tx, id)
		if err != nil {
			return err
		}
		if !due {
			if err := settlePublishAttempt(ctx, tx, id, SettledByLaterPrompt, now); err != nil {
				return err
			}
		}
	}
	return nil
}

// OpenPublishAttempts reports which of ids are still open: not observed,
// not retired.
func OpenPublishAttempts(ctx context.Context, tx store.Tx, ids []int64) (map[int64]bool, error) {
	open := make(map[int64]bool, len(ids))
	for _, id := range ids {
		var ok bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM publish_attempt WHERE id = ? AND `+openPublishLane+`)`, id).Scan(&ok); err != nil {
			return nil, err
		}
		open[id] = ok
	}
	return open, nil
}
