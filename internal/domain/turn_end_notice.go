package domain

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

// claimTurnEndNotice runs inside the hook writer transaction, after the
// caller's binding, policy epoch and latest launch have been checked.
func claimTurnEndNotice(ctx context.Context, tx store.Tx, principal, channel int64, cmd HookEvent, now time.Time) (string, error) {
	p := cmd.Profile
	if cmd.ContinuationActive || cmd.TurnRef == "" || p.VendorVersion == "unverified" || p.Mode == "unverified" {
		return "", nil
	}
	var eligible bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM turn t JOIN adapter_launch l
			ON l.principal_id = t.principal_id AND l.launch_ref = t.launch_ref
		WHERE t.principal_id = ?1 AND t.launch_ref = ?2 AND t.turn_ref = ?3 AND t.superseded = 0
			AND l.ended_hook_at_ms IS NULL AND l.profile = ?4
			AND t.opened_hook_at_ms BETWEEN l.started_hook_at_ms AND ?5
			AND `+store.CapabilityCovered("json_extract(l.profile,'$.adapter')", "l.profile", "'turn_end'")+`
			AND NOT EXISTS (SELECT 1 FROM turn_end_notice n
				WHERE n.principal_id = t.principal_id AND n.launch_ref = t.launch_ref AND n.turn_ref = t.turn_ref)
	)`, principal, cmd.LaunchRef, cmd.TurnRef, profileJSON(p), cmd.HookAtMS).Scan(&eligible)
	if err != nil || !eligible {
		return "", err
	}

	// Match the unread open asks in inbox. The claim records an output
	// attempt, leaving pickup and obligation state alone.
	ids, err := selectIDs(ctx, tx, `SELECT r.message_id FROM recipient r JOIN message m ON m.id = r.message_id
		WHERE r.principal_id = ? AND m.channel_id = ? AND m.kind = 'ask'
			AND r.picked_up_at_ms IS NULL AND r.message_id > 0 AND `+noClosedObligation+`
			AND NOT EXISTS (SELECT 1 FROM announcement i
				WHERE i.principal_id = r.principal_id AND i.message_id = r.message_id)
		ORDER BY r.message_id LIMIT ?`, principal, channel, OfferCap)
	if err != nil || len(ids) == 0 {
		return "", err
	}
	claim, err := tx.ExecContext(ctx, `INSERT INTO turn_end_notice (principal_id, adapter, launch_ref, turn_ref, at_ms)
		VALUES (?, ?, ?, ?, ?)`, principal, p.Adapter, cmd.LaunchRef, cmd.TurnRef, ms(now))
	if err != nil {
		return "", err
	}
	id, err := claim.LastInsertId()
	if err != nil {
		return "", err
	}
	parts := make([]string, len(ids))
	for i, message := range ids {
		if _, err := tx.ExecContext(ctx, `INSERT INTO announcement (principal_id, message_id, via, ref, at_ms)
			VALUES (?, ?, 'turn_end', ?, ?)`, principal, message, id, ms(now)); err != nil {
			return "", err
		}
		parts[i] = strconv.FormatInt(message, 10)
	}
	return "[crosshall] pending: " + strings.Join(parts, " "), nil
}
