package domain

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

func pendingPublishForMove(ctx context.Context, tx store.Tx, binding, principal int64) (int64, error) {
	var attempt int64
	err := tx.QueryRowContext(ctx, `WITH source_processes AS (
			SELECT p.process_pid, p.process_start_ms FROM publish_route_proof p
			JOIN publish_route r ON r.binding_id = p.binding_id
			WHERE r.binding_id = ?2 AND (p.generation = r.generation OR
				r.state = 'ended' AND r.reason = 'left channel' AND p.generation + 1 = r.generation)
			UNION ALL
			SELECT l.process_pid, l.process_start_identity FROM adapter_launch l
			WHERE l.rowid = (SELECT current.rowid FROM adapter_launch current
				WHERE current.principal_id = ?1 AND current.ended_hook_at_ms IS NULL
				ORDER BY current.started_hook_at_ms DESC, current.rowid DESC LIMIT 1)
				AND l.verified = 1 AND l.fenced = 0 AND l.process_pid > 0 AND l.process_start_identity > 0
		) SELECT id FROM publish_attempt
		WHERE state IN ('submitting', 'submitted', 'uncertain') AND `+openPublishLane+`
		AND (principal_id = ?1 OR lane IN (
			SELECT `+store.PublishLane()+` FROM (SELECT ?2 AS binding_id) source
			LEFT JOIN publish_route r ON r.binding_id = source.binding_id
			LEFT JOIN source_processes p ON 1
		)) ORDER BY id LIMIT 1`, principal, binding).Scan(&attempt)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return attempt, err
}

func movePublishState(ctx context.Context, tx store.Tx, source, destination int64, now time.Time) error {
	if err := retirePublishAttempts(ctx, tx, now, `binding_id = ? AND state = 'reserved'`, source); err != nil {
		return err
	}
	if err := retirePublishRoute(ctx, tx, source, "moved to another channel", now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE binding AS dst SET dnd = src.dnd, publish_quiet = src.publish_quiet,
		publish_owner = CASE WHEN src.publish_owner = 0 THEN 0 ELSE dst.publish_owner END,
		publish_resume_suspension = MAX(dst.publish_resume_suspension, src.publish_resume_suspension),
		publish_resume_outcome = MAX(dst.publish_resume_outcome, src.publish_resume_outcome),
		reach_told = src.reach_told, reach_seen = NULL
		FROM binding src WHERE src.id = ? AND dst.id = ?`, source, destination); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE publish_route_proof SET activated_at_ms = NULL WHERE binding_id = ?`, destination); err != nil {
		return err
	}
	if err := registerMovedPublishRoute(ctx, tx, source, destination, now); err != nil {
		return err
	}
	_, err := MarkReachChanges(ctx, tx, now)
	return err
}

func registerMovedPublishRoute(ctx context.Context, tx store.Tx, source, destination int64, now time.Time) error {
	var principal, bindingEpoch int64
	var adapter, launchRef string
	var verified bool
	var policyEpoch uint64
	var launchRoute publish.Route
	err := tx.QueryRowContext(ctx, `SELECT b.principal_id, b.route_epoch, policy.adapter, l.launch_ref, l.verified,
		policy.epoch, COALESCE(NULLIF(r.mode, ''), json_extract(l.profile, '$.mode'), ''),
		COALESCE(r.host_pid, 0), COALESCE(r.host_start, 0), COALESCE(r.vendor_home, '')
		FROM binding b JOIN `+currentBindingLaunch+`
		JOIN adapter_policy policy ON policy.adapter = json_extract(l.profile, '$.adapter')
		LEFT JOIN publish_route r ON r.binding_id = ? AND r.launch_ref = l.launch_ref AND r.adapter = policy.adapter
		WHERE b.id = ? AND l.fenced = 0`, source, destination).
		Scan(&principal, &bindingEpoch, &adapter, &launchRef, &verified, &policyEpoch, &launchRoute.Mode, &launchRoute.Host.PID, &launchRoute.Host.StartMS, &launchRoute.Home)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	a, ok := publish.Lookup(adapter)
	if !ok {
		return nil
	}
	sessionRef := resolveSessionKey(ctx, tx, "", principal, adapter)
	if sessionRef == "" {
		return nil
	}
	// The resume command names the selected binding, so a hint already printed
	// for the source must be eligible to print with the destination command.
	if _, err := tx.ExecContext(ctx, `UPDATE publish_suspension SET recovery_hint = ?1
		WHERE binding_id IN (SELECT id FROM binding WHERE principal_id = ?2) AND launch_ref = ?3
			AND until_ms IS NULL AND reason IN ('absorbed', 'not_submitted') AND recovery_hint <> ?1
			AND id > (SELECT publish_resume_suspension FROM binding WHERE id = ?4)`,
		store.RecoveryHintPending, principal, launchRef, destination); err != nil {
		return err
	}
	route := a.SessionRoute(sessionRef, launchRoute)
	route.Mode = launchRoute.Mode
	return upsertPublishRoute(ctx, tx, destination, bindingEpoch, principal, adapter, sessionRef, launchRef, verified,
		policyEpoch, route, ms(now), now)
}
