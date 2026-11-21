package domain

import (
	"context"
	"database/sql"
	"errors"
	"strconv"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

// typedAheadPrefix marks a turn opened by a prompt that reported the
// running turn's id. Claude Code does so for a prompt typed while a turn
// runs, and that prompt's Stop carries an id no prompt had (#163).
const typedAheadPrefix = "typed-ahead:"

func placeholder(ref string, at int64) string {
	return typedAheadPrefix + ref + "@" + strconv.FormatInt(at, 10)
}

// promptRow is the turn a prompt's id already opened in its launch.
type promptRow struct {
	found, open bool
	opened      int64
}

func turnRow(ctx context.Context, tx store.Tx, principal int64, launch, ref string) (promptRow, error) {
	var r promptRow
	err := tx.QueryRowContext(ctx, `SELECT opened_hook_at_ms, finishing_hook_at_ms IS NULL AND superseded = 0
		FROM turn WHERE principal_id = ? AND launch_ref = ? AND turn_ref = ?`, principal, launch, ref).Scan(&r.opened, &r.open)
	if errors.Is(err, sql.ErrNoRows) {
		return r, nil
	}
	r.found = err == nil
	return r, err
}

// promptReplay says a prompt repeats one already handled: its id opened a
// turn of the launch that is no longer running, or at the same time. Only a
// prompt reporting a running turn's id is typed ahead; any other repeat
// changes nothing, as a replay did before.
func promptReplay(ctx context.Context, tx store.Tx, principal int64, cmd HookEvent) (bool, error) {
	if cmd.Fact != FactPromptSubmitted || cmd.TurnRef == "" {
		return false, nil
	}
	r, err := turnRow(ctx, tx, principal, cmd.LaunchRef, cmd.TurnRef)
	return r.found && (!r.open || r.opened == cmd.HookAtMS), err
}

// soleOpenPlaceholder is the typed-ahead placeholder of the launch when it
// is the only unfinished turn opened before at since the latest prompt with
// an id of its own: then which turn an unknown id stands for is unambiguous.
// A turn before that prompt ended, with or without its Stop: a
// prompt typed while a turn runs reports that turn's id, not a new one.
func soleOpenPlaceholder(ctx context.Context, tx store.Tx, principal int64, launch string, at int64) (string, bool, error) {
	var ref string
	var n int
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(turn_ref), '') FROM turn
		WHERE principal_id = ?1 AND launch_ref = ?2 AND finishing_hook_at_ms IS NULL AND opened_hook_at_ms <= ?3
			AND opened_hook_at_ms >= COALESCE((SELECT MAX(opened_hook_at_ms) FROM turn
				WHERE principal_id = ?1 AND launch_ref = ?2 AND opened_hook_at_ms <= ?3
					AND substr(turn_ref, 1, length(?4)) <> ?4), 0)`,
		principal, launch, at, typedAheadPrefix).Scan(&n, &ref)
	if err != nil || n != 1 || len(ref) <= len(typedAheadPrefix) || ref[:len(typedAheadPrefix)] != typedAheadPrefix {
		return "", false, err
	}
	return ref, true, nil
}

// promptTurn is the turn a prompt opens, by hook entry time rather than
// arrival:
//   - its id opened a running turn earlier: it was typed ahead, and opens a
//     placeholder that the next unknown Stop adopts;
//   - its id opened a running turn later: that later packet was the typed-
//     ahead one, delivered first; it becomes the placeholder, and this
//     prompt opens its own turn;
//   - its id is new while a typed-ahead placeholder is the only running
//     turn: the id is that turn's, typed ahead in a chain; the
//     placeholder is bound to it, and this prompt opens the next one;
//   - otherwise its own id.
func promptTurn(ctx context.Context, tx store.Tx, principal int64, cmd HookEvent) (string, error) {
	r, err := turnRow(ctx, tx, principal, cmd.LaunchRef, cmd.TurnRef)
	if err != nil {
		return "", err
	}
	switch {
	case r.found && r.open && r.opened < cmd.HookAtMS:
		return placeholder(cmd.TurnRef, cmd.HookAtMS), nil
	case r.found && r.open && r.opened > cmd.HookAtMS:
		_, err := tx.ExecContext(ctx, `UPDATE turn SET turn_ref = ? WHERE principal_id = ? AND launch_ref = ? AND turn_ref = ?`,
			placeholder(cmd.TurnRef, r.opened), principal, cmd.LaunchRef, cmd.TurnRef)
		return cmd.TurnRef, err
	case !r.found:
		running, ok, err := soleOpenPlaceholder(ctx, tx, principal, cmd.LaunchRef, cmd.HookAtMS)
		if err != nil || !ok {
			return cmd.TurnRef, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE turn SET turn_ref = ? WHERE principal_id = ? AND launch_ref = ? AND turn_ref = ?`,
			cmd.TurnRef, principal, cmd.LaunchRef, running); err != nil {
			return "", err
		}
		return placeholder(cmd.TurnRef, cmd.HookAtMS), nil
	}
	return cmd.TurnRef, nil
}

// adoptTypedAhead gives a turn end whose id opened no turn of the launch the
// typed-ahead placeholder that is the only unfinished turn opened before it;
// with any other history it adopts nothing.
func adoptTypedAhead(ctx context.Context, tx store.Tx, principal int64, cmd HookEvent) error {
	if cmd.TurnRef == "" {
		return nil
	}
	if r, err := turnRow(ctx, tx, principal, cmd.LaunchRef, cmd.TurnRef); err != nil || r.found {
		return err
	}
	running, ok, err := soleOpenPlaceholder(ctx, tx, principal, cmd.LaunchRef, cmd.HookAtMS)
	if err != nil || !ok {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE turn SET turn_ref = ? WHERE principal_id = ? AND launch_ref = ? AND turn_ref = ?`,
		cmd.TurnRef, principal, cmd.LaunchRef, running)
	return err
}
