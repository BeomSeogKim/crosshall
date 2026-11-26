package domain

import (
	"context"
	"encoding/json"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/link"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

const linkWakeHeldWindow = publishHeldAlertAfter

func expireHeldLinkWakes(ctx context.Context, tx store.Tx, device string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE link_wake_attempt SET released_at_ms = ?
		WHERE device_id = ? AND released_at_ms IS NULL AND state IN ('submitted', 'uncertain') AND first_result_at_ms <= ?`,
		ms(now), device, ms(now.Add(-linkWakeHeldWindow)))
	return err
}

func recordLinkWakeActivity(ctx context.Context, tx store.Tx, principal int64, device string, credential int64,
	member link.Member, token link.HookToken, launch string, policy uint64, routeEpoch int64, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE link_wake_attempt
		SET activity_seq = max(activity_seq, ?1),
			released_at_ms = CASE WHEN state IN ('submitted', 'uncertain') AND submission_seq > 0 AND ?1 > submission_seq
				THEN ?2 ELSE NULL END
		WHERE principal_id = ?3 AND device_id = ?4 AND credential_gen = ?5 AND vendor = ?6 AND binding_id = ?7
			AND released_at_ms IS NULL AND state <> 'not_submitted'
			AND json_extract(route, '$.key') = ?8 AND json_extract(route, '$.boot') = ?9
			AND json_extract(route, '$.scope.leaf_generation') = ?10
			AND json_extract(route, '$.scope.membership.generation') = ?11
			AND (?12 = '' OR (json_extract(route, '$.launch') = ?12 AND json_extract(route, '$.policy_epoch') = ?13
				AND json_extract(route, '$.scope.route_epoch') = ?14))`,
		token.LeafSeq, ms(now), principal, device, credential, member.Vendor, token.Membership.BindingID,
		member.Key, token.Boot, token.LeafGeneration, token.Membership.Generation, launch, policy, routeEpoch)
	return err
}

func recordLinkWakePickup(ctx context.Context, tx store.Tx, member memberView, call *LinkCall, now time.Time) error {
	token := call.WakePickup
	if token == nil || token.Boot == "" || !validRef(token.Boot) ||
		token.LeafGeneration != call.LeafGeneration || token.LeafSeq == 0 || !validLinkInteger(token.LeafSeq) ||
		token.Membership.BindingID != member.binding || token.Membership.Generation != member.generation {
		return nil
	}
	return recordLinkWakeActivity(ctx, tx, member.principal, call.Device, call.CredentialGen,
		link.Member{Vendor: call.Vendor, Key: call.Key}, *token, "", 0, 0, now)
}

func checkLinkWakeEvent(ctx context.Context, tx store.Tx, device string, event *link.Event) error {
	var wake link.Wake
	if err := json.Unmarshal(event.Body, &wake); err != nil {
		return err
	}
	var open bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM link_wake_attempt a
		JOIN device d ON d.device_id = a.device_id AND d.credential_gen = a.credential_gen
		WHERE a.id = ? AND a.device_id = ? AND a.event_seq = ? AND a.state = 'dispatching' AND a.released_at_ms IS NULL)`,
		wake.AttemptID, device, event.Seq).Scan(&open); err != nil {
		return err
	}
	if open {
		return nil
	}
	body, err := json.Marshal(link.WakeClosed{AttemptID: wake.AttemptID})
	if err != nil {
		return err
	}
	event.Kind, event.Body = link.EventWakeClosed, body
	return nil
}
