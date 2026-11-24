package domain

import (
	"context"
	"errors"
	"strconv"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// After the hall restarts, a member that read a request and owes its reply
// is woken by nothing: a notice carries only unread work. Each start, the
// publisher gives each such member it can wake one resume notice naming
// what it owes (H5).

// ResumeWindow bounds how long after a start the publisher still tries to
// give a member its resume notice.
const ResumeWindow = time.Hour

// owedResumeItems selects, as recipient x of message m, the requests the
// member of route r (binding ?2) still owes in its channel that no notice
// will carry: read, already told of, or stored before the route's
// watermark. It starts from the member's open obligations (an index), so
// a member's history of received messages is never scanned.
const owedResumeItems = `FROM publish_route r JOIN binding b ON b.id = r.binding_id
	JOIN obligation o ON o.principal_id = r.principal_id AND o.state = 'open'
	JOIN recipient x ON x.message_id = o.message_id AND x.principal_id = o.principal_id
	JOIN message m ON m.id = o.message_id
	WHERE r.binding_id = ?2 AND b.state = 'live' AND m.channel_id = b.channel_id AND m.kind = 'ask'
		AND (x.picked_up_at_ms IS NOT NULL OR x.message_id <= r.watermark_message_id OR EXISTS (SELECT 1 FROM announcement a
			WHERE a.principal_id = x.principal_id AND a.message_id = x.message_id))`

// openResumeItem is BeginPublish's check of a resume attempt's item: still
// an open obligation of the member, read or not.
const openResumeItem = `EXISTS (SELECT 1 FROM obligation o WHERE o.message_id = x.message_id AND o.principal_id = x.principal_id AND o.state = 'open')`

// OwingMember is a live member that owes a reply at a start.
type OwingMember struct {
	BindingID int64  `json:"binding_id"`
	Channel   string `json:"channel"`
	Alias     string `json:"alias"`
	Owed      int    `json:"owed"`
	// Wake is what can reach it: a member the hall cannot wake (no route,
	// notices off, session ended) gets no resume notice.
	Wake Wake `json:"wake"`
}

// CanResume says the publisher will try to give it a resume notice.
func (m OwingMember) CanResume() bool {
	switch m.Wake.Why {
	case WhyNoRoute, WhyOff, WhyEnded:
		return false
	}
	return true
}

// OwingMembers lists the live members that owe at least one reply in their
// channel, with what can wake each. The publisher reads it once per start,
// and the owner's start reads it once.
func OwingMembers(ctx context.Context, tx store.Tx, now time.Time) ([]OwingMember, error) {
	rows, err := tx.QueryContext(ctx, `SELECT b.id, c.name, b.alias, COUNT(*) FROM obligation o
		JOIN message m ON m.id = o.message_id
		JOIN binding b ON b.principal_id = o.principal_id AND b.channel_id = m.channel_id AND b.state = 'live'
		JOIN channel c ON c.id = b.channel_id
		WHERE o.state = 'open' GROUP BY b.id ORDER BY c.name, b.alias`)
	if err != nil {
		return nil, err
	}
	var out []OwingMember
	for rows.Next() {
		var m OwingMember
		if err := rows.Scan(&m.BindingID, &m.Channel, &m.Alias, &m.Owed); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, m)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].Wake, err = memberWake(ctx, tx, out[i].BindingID, now); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// ReserveResume reserves the resume notice of binding: one publish attempt
// whose items are the requests it owes and already read or was told of,
// with no announcement (a notice announced them before). It reserves
// nothing while the route cannot take a notice (owes stays true: try again
// later) and nothing when the member owes no such request (owes false).
func ReserveResume(ctx context.Context, tx store.Tx, binding int64, now time.Time) (id int64, owes bool, err error) {
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 `+owedResumeItems+`)`, nil, binding).Scan(&owes); err != nil || !owes {
		return 0, owes, err
	}
	if ok, err := bindingCanReserve(ctx, tx, binding, now); !ok || err != nil {
		return 0, true, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO publish_attempt (
		binding_id, principal_id, binding_epoch, generation, adapter, session_ref, launch_ref,
		policy_epoch, process_pid, process_start_ms, lane, state, reserved_at_ms)
		SELECT r.binding_id, r.principal_id, r.binding_epoch, r.generation, r.adapter, r.session_ref, r.launch_ref,
			r.policy_epoch, p.process_pid, p.process_start_ms, `+store.PublishLane()+`, 'reserved', ?1 `+activePublishRoutes(now)+`
		AND r.binding_id = ?2
		AND (SELECT COUNT(*) FROM publish_attempt a WHERE a.principal_id = r.principal_id AND
			(a.state = 'reserved' OR (a.submitting_at_ms > ?3 AND a.state IN ('submitting', 'submitted', 'uncertain')))) < ?4
		ON CONFLICT DO NOTHING`, ms(now), binding, ms(now.Add(-time.Hour)), store.PublishHourlyCap)
	if err != nil {
		return 0, true, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return 0, true, err
	}
	if id, err = res.LastInsertId(); err != nil {
		return 0, true, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO publish_attempt_item (attempt_id, principal_id, message_id)
		SELECT ?1, x.principal_id, x.message_id `+owedResumeItems+` ORDER BY x.message_id LIMIT ?3`, id, binding, OfferCap); err != nil {
		return 0, true, err
	}
	items, err := publishAttemptIDs(ctx, tx, id)
	if err != nil {
		return 0, true, err
	}
	var principal int64
	if err := tx.QueryRowContext(ctx, `SELECT principal_id FROM publish_attempt WHERE id = ?`, id).Scan(&principal); err != nil {
		return 0, true, err
	}
	// For the owner's views only: without it a resume reads as a notice of
	// read messages. Nothing reads it back.
	return id, true, audit(ctx, tx, "publish_resume", "binding:"+strconv.FormatInt(binding, 10), principal,
		map[string]any{"binding_id": binding, "attempt": id, "items": items}, now)
}

// BeginResume is BeginPublish for a resume attempt: its items must still be
// open obligations of the member, read or not.
func BeginResume(ctx context.Context, tx store.Tx, id int64, now time.Time) (PublishTarget, []int64, error) {
	return beginPublish(ctx, tx, id, openResumeItem, now)
}

const VerbOwnerOwed = "owner owed"

// OwnerOwed lists the members that owe a reply, for the line owner start
// prints after the dispatcher is ready.
type OwnerOwed struct{}

func (OwnerOwed) command()     {}
func (OwnerOwed) Verb() string { return VerbOwnerOwed }

type OwnerOwedResult struct {
	Members []OwingMember `json:"members"`
}

func (s *Service) ownerOwed(ctx context.Context, tx store.Tx, caller core.Caller, now time.Time) (core.Result, error) {
	if caller.Role != core.RoleOwner {
		return Refuse(core.CodeOwnerOnly, "only the owner can list what members owe", "crosshall inbox"), nil
	}
	members, err := OwingMembers(ctx, tx, now)
	if err != nil {
		return core.Result{}, err
	}
	return OK(OwnerOwedResult{Members: members})
}
