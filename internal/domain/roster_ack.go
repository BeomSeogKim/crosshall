package domain

import (
	"context"
	"database/sql"
	"errors"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

// AcknowledgeRoster applies a member's roster receipts to its binding; the
// dispatcher does so before the handler reads the roster. The undo
// restores just these fields on an enrolled refusal, leaving its audit intact.
// A member a hook joined owes its rules until the CLI recorded printing them
// (rules): a roster cursor alone, which an older version also wrote after a
// reply without rules, settles them only for a member that joined by hand.
func AcknowledgeRoster(ctx context.Context, tx store.Tx, bindingID *int64, cursor, rules uint64) (uint64, func() error, error) {
	if bindingID == nil || (cursor == 0 && rules == 0) {
		return 0, nil, nil
	}
	var previous, latest int64
	var hinted, hooked bool
	err := tx.QueryRowContext(ctx, `SELECT roster_ack_seq, hint_acked, roster_baseline_seq IS NOT NULL,
		(SELECT COALESCE(max(seq), 0) FROM roster_change WHERE channel_id = binding.channel_id)
		FROM binding WHERE id = ? AND state = 'live'`, *bindingID).Scan(&previous, &hinted, &hooked, &latest)
	if errors.Is(err, sql.ErrNoRows) && cursor == 0 {
		return 0, nil, nil // only a receipt, and the binding it was for is not live
	}
	if err != nil {
		return 0, nil, err
	}
	bounded := min(cursor, uint64(max(latest, 0)))
	seen := bounded
	if hooked {
		seen = min(rules, uint64(max(latest, 0)))
	}
	acked, given := max(previous, int64(bounded), int64(seen)), hinted || seen > 0
	if acked == previous && given == hinted {
		return bounded, nil, nil
	}
	if _, err := tx.ExecContext(ctx, "UPDATE binding SET roster_ack_seq = ?, hint_acked = ? WHERE id = ?",
		acked, given, *bindingID); err != nil {
		return 0, nil, err
	}
	return bounded, func() error {
		_, err := tx.ExecContext(ctx, "UPDATE binding SET roster_ack_seq = ?, hint_acked = ? WHERE id = ?",
			previous, hinted, *bindingID)
		return err
	}, nil
}
