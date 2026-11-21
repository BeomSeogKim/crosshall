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

// promptRow is the turn a prompt's id already opened in its launch, with
// its recorded hook times.
type promptRow struct {
	found     bool
	opened    int64
	finishing sql.NullInt64
}

func turnRow(ctx context.Context, tx store.Tx, principal int64, launch, ref string) (promptRow, error) {
	var r promptRow
	err := tx.QueryRowContext(ctx, `SELECT opened_hook_at_ms, finishing_hook_at_ms
		FROM turn WHERE principal_id = ? AND launch_ref = ? AND turn_ref = ?`, principal, launch, ref).Scan(&r.opened, &r.finishing)
	if errors.Is(err, sql.ErrNoRows) {
		return r, nil
	}
	r.found = err == nil
	return r, err
}

// runningAt says the id's turn was running at the hook time at, by the
// recorded hook times rather than by what has been delivered so far: it
// opened before at, its Stop (if any) came after at, and no other turn of
// the launch opened in between. A second prompt with the id after a typed-
// ahead one is not typed ahead again: Claude Code merges such inputs.
func runningAt(ctx context.Context, tx store.Tx, principal int64, launch, ref string, r promptRow, at int64) (bool, error) {
	if !r.found || r.opened >= at || (r.finishing.Valid && r.finishing.Int64 <= at) {
		return false, nil
	}
	var between bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM turn WHERE principal_id = ? AND launch_ref = ?
		AND turn_ref <> ? AND opened_hook_at_ms > ? AND opened_hook_at_ms < ?)`, principal, launch, ref, r.opened, at).Scan(&between)
	return !between, err
}

// promptReplay says a prompt repeats one already handled: its id opened a
// turn of the launch earlier that was not running at the prompt's hook
// time, or opened at that very time. Only a prompt reporting a running
// turn's id is typed ahead; any other repeat changes nothing, as a replay
// did before. Delivery order does not matter: a typed-ahead prompt
// delivered after its turn's Stop is still typed ahead.
func promptReplay(ctx context.Context, tx store.Tx, principal int64, cmd HookEvent) (bool, error) {
	if cmd.Fact != FactPromptSubmitted || cmd.TurnRef == "" {
		return false, nil
	}
	r, err := turnRow(ctx, tx, principal, cmd.LaunchRef, cmd.TurnRef)
	if err != nil || !r.found || r.opened > cmd.HookAtMS {
		return false, err
	}
	running, err := runningAt(ctx, tx, principal, cmd.LaunchRef, cmd.TurnRef, r, cmd.HookAtMS)
	return !running, err
}

// solePlaceholderAt is the typed-ahead placeholder of the launch when it was
// the only turn running at the hook time at since the latest prompt with an
// id of its own opened by then: then which turn an unknown id stands for is
// unambiguous. A turn whose recorded Stop is later than at was still
// running at at, whenever that Stop was delivered. A turn before the latest
// prompt with its own id ended, with or without its Stop: a prompt typed
// while a turn runs reports that turn's id, not a new one.
func solePlaceholderAt(ctx context.Context, tx store.Tx, principal int64, launch string, at int64) (string, bool, error) {
	var ref string
	var n int
	var unfinished bool
	err := tx.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(MAX(turn_ref), ''), COALESCE(MAX(finishing_hook_at_ms IS NULL), 0) FROM turn
		WHERE principal_id = ?1 AND launch_ref = ?2 AND opened_hook_at_ms <= ?3
			AND (finishing_hook_at_ms IS NULL OR finishing_hook_at_ms > ?3)
			AND opened_hook_at_ms >= COALESCE((SELECT MAX(opened_hook_at_ms) FROM turn
				WHERE principal_id = ?1 AND launch_ref = ?2 AND opened_hook_at_ms <= ?3
					AND substr(turn_ref, 1, length(?4)) <> ?4), 0)`,
		principal, launch, at, typedAheadPrefix).Scan(&n, &ref, &unfinished)
	if err != nil || n != 1 || !unfinished || len(ref) <= len(typedAheadPrefix) || ref[:len(typedAheadPrefix)] != typedAheadPrefix {
		return "", false, err
	}
	return ref, true, nil
}

// promptTurn is the turn a prompt opens, by recorded hook times rather than
// arrival:
//   - its id's turn was running at the prompt's hook time: it was typed
//     ahead, and opens a placeholder that the next unknown Stop adopts;
//   - its id opened a turn at a later hook time: that later packet was the
//     typed-ahead one, delivered first; it becomes the placeholder, and this
//     prompt opens its id's own turn, taking the id's Stop if one came;
//   - its id is new while a typed-ahead placeholder was the only running
//     turn: the id is that turn's, typed ahead in a chain; the placeholder
//     is bound to it, and this prompt opens the next one;
//   - otherwise its own id.
func promptTurn(ctx context.Context, tx store.Tx, principal int64, cmd HookEvent) (string, error) {
	r, err := turnRow(ctx, tx, principal, cmd.LaunchRef, cmd.TurnRef)
	if err != nil {
		return "", err
	}
	switch {
	case r.found && r.opened < cmd.HookAtMS:
		return placeholder(cmd.TurnRef, cmd.HookAtMS), nil // promptReplay let only a running turn's id through
	case r.found && r.opened > cmd.HookAtMS:
		if _, err := tx.ExecContext(ctx, `UPDATE turn SET turn_ref = ?, finishing_hook_at_ms = NULL
			WHERE principal_id = ? AND launch_ref = ? AND turn_ref = ?`,
			placeholder(cmd.TurnRef, r.opened), principal, cmd.LaunchRef, cmd.TurnRef); err != nil {
			return "", err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO turn (principal_id, launch_ref, turn_ref, opened_hook_at_ms, finishing_hook_at_ms, superseded)
			VALUES (?, ?, ?, ?, ?, 1)`, principal, cmd.LaunchRef, cmd.TurnRef, cmd.HookAtMS, r.finishing)
		return cmd.TurnRef, err
	case !r.found:
		running, ok, err := solePlaceholderAt(ctx, tx, principal, cmd.LaunchRef, cmd.HookAtMS)
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
// typed-ahead placeholder that was the only turn running at its hook time;
// with any other history it adopts nothing.
func adoptTypedAhead(ctx context.Context, tx store.Tx, principal int64, cmd HookEvent) error {
	if cmd.TurnRef == "" {
		return nil
	}
	if r, err := turnRow(ctx, tx, principal, cmd.LaunchRef, cmd.TurnRef); err != nil || r.found {
		return err
	}
	running, ok, err := solePlaceholderAt(ctx, tx, principal, cmd.LaunchRef, cmd.HookAtMS)
	if err != nil || !ok {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE turn SET turn_ref = ? WHERE principal_id = ? AND launch_ref = ? AND turn_ref = ?`,
		cmd.TurnRef, principal, cmd.LaunchRef, running)
	return err
}

// rebindTypedAhead applies a chain binding that a late delivery made
// decidable: a prompt with a new id that opened its own turn because, as
// delivered so far, the history was ambiguous, while by the recorded hook
// times a typed-ahead placeholder was the only turn running just before it.
// The placeholder takes that id (with its Stop, if one came), and the
// prompt's turn becomes the next placeholder. A turn that already gave a
// turn-end notice is left as it is.
func rebindTypedAhead(ctx context.Context, tx store.Tx, principal int64, launch string) error {
	rows, err := tx.QueryContext(ctx, `SELECT p.turn_ref, y.turn_ref, y.opened_hook_at_ms, y.finishing_hook_at_ms, y.superseded
		FROM turn p JOIN turn y ON y.principal_id = p.principal_id AND y.launch_ref = p.launch_ref
		WHERE p.principal_id = ?1 AND p.launch_ref = ?2 AND p.finishing_hook_at_ms IS NULL
			AND substr(p.turn_ref, 1, length(?3)) = ?3 AND substr(y.turn_ref, 1, length(?3)) <> ?3
			AND y.opened_hook_at_ms = (SELECT MIN(opened_hook_at_ms) FROM turn
				WHERE principal_id = ?1 AND launch_ref = ?2 AND opened_hook_at_ms > p.opened_hook_at_ms)
			AND NOT EXISTS (SELECT 1 FROM turn_end_notice n
				WHERE n.principal_id = y.principal_id AND n.launch_ref = y.launch_ref AND n.turn_ref = y.turn_ref)
		ORDER BY p.opened_hook_at_ms LIMIT 1`, principal, launch, typedAheadPrefix)
	if err != nil {
		return err
	}
	var running, ref string
	var opened int64
	var finishing sql.NullInt64
	var superseded, found bool
	if rows.Next() {
		found = true
		err = rows.Scan(&running, &ref, &opened, &finishing, &superseded)
	}
	if err := errors.Join(err, rows.Err(), rows.Close()); err != nil || !found {
		return err
	}
	sole, ok, err := solePlaceholderAt(ctx, tx, principal, launch, opened-1)
	if err != nil || !ok || sole != running {
		return err
	}
	next := placeholder(ref, opened)
	for _, step := range []struct {
		query string
		args  []any
	}{
		{`UPDATE turn SET turn_ref = ?, finishing_hook_at_ms = NULL, superseded = ? WHERE principal_id = ? AND launch_ref = ? AND turn_ref = ?`,
			[]any{next, superseded, principal, launch, ref}},
		{`UPDATE turn SET turn_ref = ?, finishing_hook_at_ms = ?, superseded = 1 WHERE principal_id = ? AND launch_ref = ? AND turn_ref = ?`,
			[]any{ref, finishing, principal, launch, running}},
	} {
		if _, err := tx.ExecContext(ctx, step.query, step.args...); err != nil {
			return err
		}
	}
	return nil
}

// applyRecordedStops gives a turn the Stop recorded for its id before the
// turn had a row (its prompt delivered after its Stop, or its placeholder
// bound to the id later): the earliest Stop of the id at or after the turn
// opened.
func applyRecordedStops(ctx context.Context, tx store.Tx, principal int64, launch string) error {
	_, err := tx.ExecContext(ctx, `UPDATE turn SET finishing_hook_at_ms = (SELECT MIN(o.hook_at_ms) FROM observation o
			WHERE o.principal_id = turn.principal_id AND o.launch_ref = turn.launch_ref AND o.kind = 'turn_finishing'
				AND o.turn_ref = turn.turn_ref AND o.hook_at_ms >= turn.opened_hook_at_ms)
		WHERE principal_id = ? AND launch_ref = ? AND finishing_hook_at_ms IS NULL`, principal, launch)
	return err
}
