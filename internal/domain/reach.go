package domain

import (
	"context"
	"database/sql"
	"errors"
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
// notice by age.
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
		out.ReachAck = &wire.HookReachAck{BindingID: b.id, RouteEpoch: epoch, Told: 0}
	}
	return nil
}
