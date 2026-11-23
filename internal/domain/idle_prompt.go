package domain

import (
	"context"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

// currentIdlePrompt reports whether an idle fact belongs to the member's
// current launch: a bound launch that has started by the fact's time and is
// the latest one not ended. Any other idle fact is dropped unstored.
func currentIdlePrompt(ctx context.Context, tx store.Tx, principal int64, cmd HookEvent, now time.Time) (bool, error) {
	if cmd.HookAtMS > ms(now)+futureSlackMS {
		return false, nil
	}
	var current bool
	err := tx.QueryRowContext(ctx, `SELECT COALESCE((
		SELECT launch_ref = ? AND verified = 1 AND fenced = 0 AND started_hook_at_ms <= ?
		FROM adapter_launch WHERE principal_id = ? AND ended_hook_at_ms IS NULL
		ORDER BY started_hook_at_ms DESC, rowid DESC LIMIT 1), 0)`,
		cmd.LaunchRef, cmd.HookAtMS, principal).Scan(&current)
	return current, err
}

// applyIdlePrompt applies the vendor's report that the session waits for
// input: no turn runs, so the ledger closes the launch's open typed-ahead
// placeholders (a prompt absorbed into a running turn gets no Stop of its
// own), and an unknown suspension of the launch committed before the fact
// ends now instead of running its thirty minutes (#236).
func applyIdlePrompt(ctx context.Context, tx store.Tx, principal int64, cmd HookEvent, now time.Time) error {
	if _, _, err := reconcileTurns(ctx, tx, principal, cmd.LaunchRef, cmd.EventRef, ms(now)); err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `UPDATE publish_suspension SET until_ms = ?
		WHERE binding_id IN (SELECT id FROM binding WHERE principal_id = ?)
			AND launch_ref = ? AND reason = 'unknown' AND at_ms < ?
			AND (until_ms IS NULL OR until_ms > ?)`,
		ms(now), principal, cmd.LaunchRef, cmd.HookAtMS, ms(now))
	if err != nil {
		return err
	}
	changed, err := res.RowsAffected()
	if err != nil || changed == 0 {
		return err
	}
	_, err = MarkReachChanges(ctx, tx, now)
	return err
}
