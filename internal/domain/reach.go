package domain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// Rule 1 lifts the end-of-turn check for a member while the hall's last word
// to it was ReachBringsLine.
const (
	ReachBringsLine = "[crosshall] The hall brings new work to this session: no check is needed before ending a turn."
	ReachLostLine   = "[crosshall] The hall cannot reach this session now: check before ending a turn (crosshall read)."
)

// HeldReachBound is how long a notice held for a member, its receipt not
// yet seen, still counts as the hall reaching it. Nothing else ends a held
// notice by age, and no event marks the crossing: the dispatcher's sweep
// does, so a member relies on HeldReachBound plus one sweep period.
const HeldReachBound = 10 * time.Minute

// ReachesForNewWork holds when the hall starts the member's turn for work
// that arrives: its route is available, or a notice was queued for it less
// than HeldReachBound ago.
func ReachesForNewWork(state store.PublishState, now time.Time) bool {
	switch state.State {
	case "available":
		return true
	case "held":
		return now.UnixMilli()-state.AtMS < HeldReachBound.Milliseconds()
	}
	return false
}

// offerReach adds the reach line when what the member was last told no
// longer holds; after a context reset (restored) a "brings" is said again.
// The stored value changes only when the hook confirms the line.
func offerReach(ctx context.Context, tx store.Tx, b hookBinding, restored bool, out *wire.HookResult, now time.Time) error {
	if b.dnd {
		return nil
	}
	var epoch int64
	var told sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT route_epoch, reach_told FROM binding WHERE id = ? AND state = 'live'`,
		b.id).Scan(&epoch, &told); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	state, found, err := store.ReadPublishState(ctx, tx, b.id, now)
	if err != nil {
		return err
	}
	reach := found && ReachesForNewWork(state, now)
	toldBrings := told.Valid && told.Int64 == 1
	switch {
	case reach && (!toldBrings || restored):
		out.ReachLine = ReachBringsLine
		out.ReachAck = &wire.HookReachAck{BindingID: b.id, RouteEpoch: epoch, Told: 1}
	case !reach && toldBrings:
		out.ReachLine = ReachLostLine
		if state.LastingLaunchSuspension {
			out.ReachLine += fmt.Sprintf(" Notices stay off until this session restarts. Your user can turn them on with crosshall owner publish resume %d.", b.id)
		}
		out.ReachAck = &wire.HookReachAck{BindingID: b.id, RouteEpoch: epoch, Told: 0}
	}
	return nil
}

// reachDue holds when what a member was last told no longer holds now.
func reachDue(reach bool, told sql.NullInt64) bool {
	toldBrings := told.Valid && told.Int64 == 1
	return reach != toldBrings && (reach || told.Valid)
}

// MarkReachChanges moves the notice stamp of each live member whose reach
// changed since the dispatcher last looked and differs from what it was
// told, so that its next prompt asks the dispatcher instead of taking the
// hook's clean path, and is told. It decides by state, not by event: the
// sites that change reach call it in their own transaction, and the
// dispatcher's sweep calls it as the net, also for changes that have no
// event (a held notice outliving HeldReachBound, a suspension or the cap
// ending). A member never reached and never told is never marked.
func MarkReachChanges(ctx context.Context, tx store.Tx, now time.Time) (bool, error) {
	type member struct {
		binding, principal int64
		told, seen         sql.NullInt64
	}
	rows, err := tx.QueryContext(ctx, `SELECT b.id, b.principal_id, b.reach_told, b.reach_seen FROM binding b
		JOIN publish_route r ON r.binding_id = b.id WHERE b.state = 'live'`)
	if err != nil {
		return false, err
	}
	var members []member
	for rows.Next() {
		var m member
		if err := rows.Scan(&m.binding, &m.principal, &m.told, &m.seen); err != nil {
			rows.Close()
			return false, err
		}
		members = append(members, m)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return false, err
	}
	changed := false
	for _, m := range members {
		state, found, err := store.ReadPublishState(ctx, tx, m.binding, now)
		if err != nil {
			return false, err
		}
		reach := found && ReachesForNewWork(state, now)
		seen := int64(0)
		if reach {
			seen = 1
		}
		if m.seen.Valid && m.seen.Int64 == seen {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE binding SET reach_seen = ? WHERE id = ?`, seen, m.binding); err != nil {
			return false, err
		}
		// Once marked, the next prompt asks, and keeps asking until the
		// hook confirms the line: marking again would add nothing.
		if !reachDue(reach, m.told) {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO adapter_dirty (principal_id, notice) VALUES (?, 1)
			ON CONFLICT(principal_id) DO UPDATE SET notice = 1`, m.principal); err != nil {
			return false, err
		}
		changed = true
	}
	return changed, nil
}
