package domain

import (
	"context"
	"strconv"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

// typedAheadPrefix marks a turn opened by a prompt that reported the
// previous turn's id. Claude Code does so for a prompt typed while the
// previous turn runs, and that prompt's Stop carries an id no prompt had
// (#163).
const typedAheadPrefix = "typed-ahead:"

// promptTurn is the turn a prompt opens: its own reference, or, when that
// reference already opened a turn of the launch earlier, a placeholder for
// a typed-ahead prompt, which its turn's end adopts.
func promptTurn(ctx context.Context, tx store.Tx, principal int64, cmd HookEvent) (string, error) {
	var repeated bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM turn WHERE principal_id = ? AND launch_ref = ?
		AND turn_ref = ? AND opened_hook_at_ms < ?)`, principal, cmd.LaunchRef, cmd.TurnRef, cmd.HookAtMS).Scan(&repeated); err != nil || !repeated {
		return cmd.TurnRef, err
	}
	return typedAheadPrefix + cmd.TurnRef + "@" + strconv.FormatInt(cmd.HookAtMS, 10), nil
}

// adoptTypedAhead gives a turn end whose reference opened no turn of the
// launch the latest unfinished typed-ahead placeholder opened before it.
func adoptTypedAhead(ctx context.Context, tx store.Tx, principal int64, cmd HookEvent) error {
	if cmd.TurnRef == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE turn SET turn_ref = ?3 WHERE rowid = (
		SELECT rowid FROM turn WHERE principal_id = ?1 AND launch_ref = ?2
			AND substr(turn_ref, 1, length(?5)) = ?5 AND finishing_hook_at_ms IS NULL AND opened_hook_at_ms <= ?4
		ORDER BY opened_hook_at_ms DESC, rowid DESC LIMIT 1)
		AND NOT EXISTS (SELECT 1 FROM turn WHERE principal_id = ?1 AND launch_ref = ?2 AND turn_ref = ?3)`,
		principal, cmd.LaunchRef, cmd.TurnRef, cmd.HookAtMS, typedAheadPrefix)
	return err
}
