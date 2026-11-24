package domain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

type PublishBinding struct {
	ID         int64
	Generation int64
}

type (
	PublishQueue   = publish.Queue
	PublishTarget  = publish.Target
	PublishOutcome = publish.Outcome
)

const (
	PublishNotSubmitted = publish.NotSubmitted
	PublishSubmitted    = publish.Submitted
	PublishUncertain    = publish.Uncertain
)

// activePublishRoutes selects the routes a notice may use at now. Their
// adapter must also resolve each route's address (routeAddress).
func activePublishRoutes(now time.Time) string { return publishRoutes(now, true) }

// publishRoutes selects them with or without the transport's proof of the
// process that owns the address.
func publishRoutes(now time.Time, proven bool) string {
	proof := `JOIN publish_route_proof p ON p.binding_id = r.binding_id AND p.generation = r.generation
		AND p.activated_at_ms IS NOT NULL`
	if !proven {
		proof = ""
	}
	return `FROM publish_route r
	JOIN binding b ON b.id = r.binding_id AND b.principal_id = r.principal_id AND b.route_epoch = r.binding_epoch
	` + proof + `
	JOIN adapter_policy policy ON policy.adapter = r.adapter AND policy.epoch = r.policy_epoch AND policy.enabled = 1
	JOIN adapter_launch l ON l.rowid = (SELECT current.rowid FROM adapter_launch current
		WHERE current.principal_id = r.principal_id AND current.ended_hook_at_ms IS NULL
		ORDER BY current.started_hook_at_ms DESC, current.rowid DESC LIMIT 1)
	WHERE ` + store.PublishAdapterSends() + ` AND r.state = 'active' AND ` + store.PublishLaunchBinds() + `
		AND b.state = 'live' AND b.dnd = 0
		AND ` + store.PublishSwitchesOn + ` AND ` + store.PublishNotSuspended(ms(now)) + ` AND ` + store.PublishProfileCovered
}

const publishMessages = `SELECT x.message_id FROM recipient x JOIN message m ON m.id = x.message_id
	WHERE x.principal_id = r.principal_id AND m.channel_id = b.channel_id
		AND x.message_id > r.watermark_message_id AND x.picked_up_at_ms IS NULL
		AND (m.kind = 'ask' AND EXISTS (SELECT 1 FROM obligation o
			WHERE o.message_id = x.message_id AND o.principal_id = x.principal_id AND o.state = 'open')
			OR m.kind = 'reply' AND m.outcome IN ('answer', 'decline') AND EXISTS (
				SELECT 1 FROM message ask WHERE ask.id = m.in_reply_to AND ask.kind = 'ask' AND ask.sender_id = x.principal_id))
		AND NOT EXISTS (SELECT 1 FROM announcement a WHERE a.principal_id = x.principal_id AND a.message_id = x.message_id)
		AND NOT EXISTS (SELECT 1 FROM publish_attempt_item i JOIN publish_attempt a ON a.id = i.attempt_id
			WHERE i.principal_id = x.principal_id AND i.message_id = x.message_id AND a.binding_id = r.binding_id
				AND a.generation = r.generation AND a.state = 'not_submitted')`

const openPublishLane = `observed_at_ms IS NULL AND retired_at_ms IS NULL AND state <> 'not_submitted'`

// PublishReaches is the one test of whether the hall can start a member's
// turn for new work: its route is available now, so a notice can be
// reserved for it. Such a member is not told to stand by with wait.
func PublishReaches(state store.PublishState) bool {
	return state.State == "available"
}

// publishDelivers reads a binding's publish state and applies PublishReaches.
func publishDelivers(ctx context.Context, tx store.Tx, binding int64, now time.Time) (bool, error) {
	status, _, err := store.ReadPublishState(ctx, tx, binding, now)
	return PublishReaches(status), err
}

// ConfirmPublishRoute records a transport's positive proof for this
// generation. The publisher goes through ConfirmPublishProof.
// It records identity only: a new listener waits for ActivatePublishRoutes,
// while a new generation on the process that already activated keeps its
// activation and ordinary backlog.
func ConfirmPublishRoute(ctx context.Context, tx store.Tx, binding PublishBinding, queue PublishQueue) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO publish_route_proof (binding_id, generation, process_pid, process_start_ms)
		SELECT r.binding_id, r.generation, ?1, ?2 FROM publish_route r
		WHERE r.binding_id = ?3 AND r.generation = ?4 AND r.state = 'active' AND (r.launch_verified = 1 OR `+store.PublishThreadRouted()+`)
		ON CONFLICT(binding_id) DO UPDATE SET generation = excluded.generation,
			process_pid = excluded.process_pid, process_start_ms = excluded.process_start_ms,
			activated_at_ms = CASE WHEN publish_route_proof.process_pid = excluded.process_pid
				AND publish_route_proof.process_start_ms = excluded.process_start_ms
				THEN publish_route_proof.activated_at_ms END
		WHERE publish_route_proof.generation < excluded.generation`,
		queue.PID, queue.StartMS, binding.ID, binding.Generation)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE adapter_launch AS l SET process_pid = ?1, process_start_identity = ?2
		WHERE l.process_pid IS NULL AND l.verified = 1 AND l.fenced = 0 AND l.ended_hook_at_ms IS NULL
		AND (l.launch_ref = 'pid-' || ?1 OR l.launch_ref GLOB 'pid-' || ?1 || '-*')
		AND EXISTS (SELECT 1 FROM publish_route r JOIN publish_route_proof p ON p.binding_id = r.binding_id AND p.generation = r.generation
			WHERE r.binding_id = ?3 AND r.generation = ?4 AND r.state = 'active' AND r.launch_verified = 1
			AND r.principal_id = l.principal_id AND r.launch_ref = l.launch_ref AND p.process_pid = ?1 AND p.process_start_ms = ?2)`,
		queue.PID, queue.StartMS, binding.ID, binding.Generation)
	return err
}

// ActivatePublishRoutes activates each proven route at the first moment it
// is available, the verdict status shows: its watermark becomes the newest
// message, so only work stored from then on is offered. A route that is
// off, suspended, capped or held stays unactivated, and status keeps
// showing that state, so no message stored meanwhile was announced as
// queued.
func ActivatePublishRoutes(ctx context.Context, tx store.Tx, now time.Time) error {
	bindings, err := selectIDs(ctx, tx, `SELECT p.binding_id FROM publish_route_proof p
		JOIN publish_route r ON r.binding_id = p.binding_id AND r.generation = p.generation
		WHERE p.activated_at_ms IS NULL ORDER BY p.binding_id`)
	if err != nil {
		return err
	}
	for _, binding := range bindings {
		status, _, err := store.ReadPublishState(ctx, tx, binding, now)
		if err != nil {
			return err
		}
		if status.State != "candidate" || status.Reason != store.PublishNotActivated {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE publish_route SET watermark_message_id = (SELECT COALESCE(MAX(id), 0) FROM message)
			WHERE binding_id = ?`, binding); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE publish_route_proof SET activated_at_ms = ? WHERE binding_id = ?`,
			ms(now), binding); err != nil {
			return err
		}
	}
	return nil
}

const publishRouteColumns = `r.adapter, COALESCE(r.mode, ''), COALESCE(r.socket_path, ''), COALESCE(r.thread_id, ''),
	COALESCE(r.host_pid, 0), COALESCE(r.host_start, 0), COALESCE(r.vendor_home, '')`

// routeScan is where a row's publishRouteColumns are scanned to.
func routeScan(adapter *string, route *publish.Route) []any {
	return []any{adapter, &route.Mode, &route.SocketPath, &route.ThreadID, &route.Host.PID, &route.Host.StartMS, &route.Home}
}

// routeAddress resolves a selected route's address through its adapter.
func routeAddress(adapter string, route publish.Route) (string, bool) {
	address, reason := publish.Adapters().Address(adapter, route)
	return address, reason == ""
}

// bindingRouteResolves reports whether the binding's route resolves to an
// address now.
func bindingRouteResolves(ctx context.Context, tx store.Tx, binding int64) (bool, error) {
	var adapter string
	var route publish.Route
	err := tx.QueryRowContext(ctx, `SELECT `+publishRouteColumns+` FROM publish_route r WHERE r.binding_id = ?`, binding).
		Scan(routeScan(&adapter, &route)...)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	_, ok := routeAddress(adapter, route)
	return ok && err == nil, err
}

// PublishProof is a route that could publish but for the transport's
// proof of the process that listens on its address.
type PublishProof struct {
	Binding PublishBinding
	Probe   publish.Probe
}

// UnprovenPublishRoutes lists the routes that wait only for that proof, so
// the adapter can prove them before any notice is reserved. A route proves
// before it has work: proof moves its watermark, which would hide work
// that was waiting for it.
func UnprovenPublishRoutes(ctx context.Context, tx store.Tx, now time.Time) ([]PublishProof, error) {
	rows, err := tx.QueryContext(ctx, `SELECT r.binding_id, r.generation, `+publishRouteColumns+`, r.launch_ref, l.started_hook_at_ms `+
		publishRoutes(now, false)+` AND NOT EXISTS (SELECT 1 FROM publish_route_proof p
			WHERE p.binding_id = r.binding_id AND p.generation = r.generation)
		ORDER BY r.binding_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var proofs []PublishProof
	for rows.Next() {
		var proof PublishProof
		probe := &proof.Probe
		dest := append([]any{&proof.Binding.ID, &proof.Binding.Generation}, routeScan(&probe.Adapter, &probe.Route)...)
		if err := rows.Scan(append(dest, &probe.LaunchRef, &probe.StartedHookAtMS)...); err != nil {
			return nil, err
		}
		var ok bool
		if probe.Address, ok = routeAddress(probe.Adapter, probe.Route); ok {
			proofs = append(proofs, proof)
		}
	}
	return proofs, rows.Err()
}

// launchOwns reports whether the proven listener is the probe's launch: the process its
// launch reference names, or for a thread-routed adapter the server its
// start named, started no later than the launch's first hook.
func launchOwns(probe publish.Probe, proven publish.Proven) bool {
	if proven.Queue.PID <= 0 || proven.Queue.StartMS <= 0 || proven.StartedAtMS <= 0 || proven.StartedAtMS > probe.StartedHookAtMS {
		return false
	}
	if a, ok := publish.Lookup(probe.Adapter); ok && a.ThreadRouted() {
		return proven.Queue == probe.Route.Host
	}
	process, ok := ident.LaunchProcess(probe.LaunchRef)
	return ok && process == fmt.Sprintf("pid-%d", proven.Queue.PID)
}

// ConfirmPublishProof records a transport's proof for a route only if the
// proven process is the expected launch and, in this transaction, the
// route is still unproven on the same generation, launch and resolved
// address the proof was asked for. It records identity only; whether and
// from which message the route publishes is ActivatePublishRoutes'.
func ConfirmPublishProof(ctx context.Context, tx store.Tx, proof PublishProof, proven publish.Proven) (bool, error) {
	if !launchOwns(proof.Probe, proven) {
		return false, nil
	}
	var route publish.Probe
	err := tx.QueryRowContext(ctx, `SELECT `+publishRouteColumns+`, r.launch_ref, l.started_hook_at_ms FROM publish_route r
		JOIN adapter_launch l ON l.rowid = (SELECT current.rowid FROM adapter_launch current
			WHERE current.principal_id = r.principal_id AND current.ended_hook_at_ms IS NULL
			ORDER BY current.started_hook_at_ms DESC, current.rowid DESC LIMIT 1)
		WHERE r.binding_id = ? AND r.generation = ? AND r.state = 'active' AND `+store.PublishLaunchBinds()+`
			AND (l.process_pid IS NULL OR (l.process_pid = ? AND l.process_start_identity = ?))
			AND NOT EXISTS (SELECT 1 FROM publish_route_proof p WHERE p.binding_id = r.binding_id AND p.generation = r.generation)`,
		proof.Binding.ID, proof.Binding.Generation, proven.Queue.PID, proven.Queue.StartMS).
		Scan(append(routeScan(&route.Adapter, &route.Route), &route.LaunchRef, &route.StartedHookAtMS)...)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	route.Address, _ = routeAddress(route.Adapter, route.Route)
	route.InstalledHome = proof.Probe.InstalledHome
	if route != proof.Probe {
		return false, nil
	}
	return true, ConfirmPublishRoute(ctx, tx, proof.Binding, proven.Queue)
}

func PublishCandidates(ctx context.Context, tx store.Tx, now time.Time) ([]PublishBinding, error) {
	rows, err := tx.QueryContext(ctx, `SELECT r.binding_id, r.generation, `+publishRouteColumns+` `+activePublishRoutes(now)+
		` AND EXISTS (`+publishMessages+`) ORDER BY r.binding_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var bindings []PublishBinding
	for rows.Next() {
		var binding PublishBinding
		var adapter string
		var route publish.Route
		if err := rows.Scan(append([]any{&binding.ID, &binding.Generation}, routeScan(&adapter, &route)...)...); err != nil {
			return nil, err
		}
		if _, ok := routeAddress(adapter, route); ok {
			bindings = append(bindings, binding)
		}
	}
	return bindings, rows.Err()
}

func HasPublishWork(ctx context.Context, tx store.Tx, binding PublishBinding, now time.Time) (bool, error) {
	if ok, err := bindingRouteResolves(ctx, tx, binding.ID); !ok || err != nil {
		return false, err
	}
	var pending bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 `+activePublishRoutes(now)+
		` AND r.binding_id = ? AND r.generation = ? AND EXISTS (`+publishMessages+`))`,
		binding.ID, binding.Generation).Scan(&pending)
	return pending, err
}

// ReservePublish reads the route and unread ledger again under the writer transaction.
func ReservePublish(ctx context.Context, tx store.Tx, binding PublishBinding, now time.Time) (bool, error) {
	if ok, err := bindingRouteResolves(ctx, tx, binding.ID); !ok || err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO publish_attempt (
		binding_id, principal_id, binding_epoch, generation, adapter, session_ref, launch_ref,
		policy_epoch, process_pid, process_start_ms, lane, state, reserved_at_ms)
		SELECT r.binding_id, r.principal_id, r.binding_epoch, r.generation, r.adapter, r.session_ref, r.launch_ref,
			r.policy_epoch, p.process_pid, p.process_start_ms, `+store.PublishLane()+`, 'reserved', ?1 `+activePublishRoutes(now)+`
		AND r.binding_id = ?2 AND r.generation = ?3
		AND EXISTS (`+publishMessages+`)
		AND (SELECT COUNT(*) FROM publish_attempt a WHERE a.principal_id = r.principal_id AND
			(a.state = 'reserved' OR (a.submitting_at_ms > ?4 AND a.state IN ('submitting', 'submitted', 'uncertain')))) < ?5
		ON CONFLICT DO NOTHING`, ms(now), binding.ID, binding.Generation, ms(now.Add(-time.Hour)), store.PublishHourlyCap)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO publish_attempt_item (attempt_id, principal_id, message_id)
		SELECT ?1, r.principal_id, elig.message_id FROM publish_route r JOIN binding b ON b.id = r.binding_id
		JOIN recipient elig ON elig.principal_id = r.principal_id
		WHERE r.binding_id = ?2 AND elig.message_id IN (`+publishMessages+` ORDER BY x.message_id LIMIT ?3)`,
		id, binding.ID, OfferCap); err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO announcement (principal_id, message_id, via, ref, at_ms)
		SELECT principal_id, message_id, 'publish', attempt_id, ? FROM publish_attempt_item WHERE attempt_id = ?`, ms(now), id)
	return err == nil, err
}

func ReservedPublishAttempts(ctx context.Context, tx store.Tx) ([]int64, error) {
	return selectIDs(ctx, tx, `SELECT id FROM publish_attempt WHERE state = 'reserved' ORDER BY id`)
}

func publishAttemptIDs(ctx context.Context, tx store.Tx, id int64) ([]int64, error) {
	return selectIDs(ctx, tx, `SELECT message_id FROM publish_attempt_item WHERE attempt_id = ? ORDER BY message_id`, id)
}

// BeginPublish is the final fence. Only its winner may call the transport.
func BeginPublish(ctx context.Context, tx store.Tx, id int64, now time.Time) (PublishTarget, []int64, error) {
	var target PublishTarget
	var route publish.Route
	err := tx.QueryRowContext(ctx, `SELECT `+publishRouteColumns+`, p.process_pid, p.process_start_ms `+
		activePublishRoutes(now)+` AND EXISTS (SELECT 1 FROM publish_attempt a WHERE a.id = ? AND a.state = 'reserved'
			AND a.binding_id = r.binding_id AND a.principal_id = r.principal_id AND a.binding_epoch = r.binding_epoch
			AND a.generation = r.generation AND a.adapter = r.adapter AND a.session_ref = r.session_ref AND a.launch_ref = r.launch_ref
			AND a.policy_epoch = r.policy_epoch AND a.process_pid = p.process_pid AND a.process_start_ms = p.process_start_ms)`, id).
		Scan(append(routeScan(&target.Adapter, &route), &target.Queue.PID, &target.Queue.StartMS)...)
	resolved := false
	if err == nil {
		target.Address, resolved = routeAddress(target.Adapter, route)
	}
	if errors.Is(err, sql.ErrNoRows) || err == nil && !resolved {
		return PublishTarget{}, nil, finishPublish(ctx, tx, id, "reserved", PublishNotSubmitted, now)
	}
	if err != nil {
		return PublishTarget{}, nil, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE publish_attempt SET state = 'submitting', submitting_at_ms = ?
		WHERE id = ? AND state = 'reserved'`, ms(now), id)
	if err != nil {
		return PublishTarget{}, nil, err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return PublishTarget{}, nil, err
	}
	ids, err := publishAttemptIDs(ctx, tx, id)
	return target, ids, err
}

func ResolvePublish(ctx context.Context, tx store.Tx, id int64, outcome PublishOutcome, now time.Time) error {
	switch outcome {
	case PublishNotSubmitted, PublishSubmitted, PublishUncertain:
	default:
		outcome = PublishUncertain
	}
	return finishPublish(ctx, tx, id, "submitting", outcome, now)
}

func finishPublish(ctx context.Context, tx store.Tx, id int64, from string, outcome PublishOutcome, now time.Time) error {
	res, err := tx.ExecContext(ctx, `UPDATE publish_attempt SET state = ?, resolved_at_ms = ?,
		outcome_seq = (SELECT COALESCE(MAX(outcome_seq), 0) + 1 FROM publish_attempt)
		WHERE id = ? AND state = ?`, outcome, ms(now), id, from)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 || outcome != PublishNotSubmitted {
		return err
	}
	// The withdrawn announcement gives its asks back to the turn-end line:
	// mark the member, so the record its Stop reads is raised (#261).
	if _, err = tx.ExecContext(ctx, `INSERT INTO adapter_dirty (principal_id, notice)
		SELECT DISTINCT principal_id, 1 FROM announcement WHERE via = 'publish' AND ref = ?
		ON CONFLICT(principal_id) DO UPDATE SET notice = 1`, id); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM announcement WHERE via = 'publish' AND ref = ?`, id); err != nil || from != "submitting" {
		return err
	}
	// Only a transport's non-submission counts: one stopped at the fence did
	// not reach the vendor. The last publishFailuresToSuspend outcomes the
	// transport gave on this launch, in the order they were recorded since
	// the owner's resume and since the launch's last suspension for this
	// reason, must all be non-submissions: a submission breaks the run.
	var failures int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM (SELECT a.state FROM publish_attempt a
		JOIN publish_attempt this ON this.id = ?1 JOIN binding b ON b.id = this.binding_id
		WHERE a.binding_id = this.binding_id AND a.launch_ref = this.launch_ref AND a.submitting_at_ms IS NOT NULL
			AND a.state IN ('submitted', 'uncertain', 'not_submitted') AND a.outcome_seq > b.publish_resume_outcome
			AND a.outcome_seq > COALESCE((SELECT MAX(n.outcome_seq) FROM publish_suspension s JOIN publish_attempt n ON n.id = s.notice
				WHERE s.binding_id = this.binding_id AND s.launch_ref = this.launch_ref AND s.reason = 'not_submitted'), 0)
		ORDER BY a.outcome_seq DESC LIMIT ?2) WHERE state = 'not_submitted'`, id, publishFailuresToSuspend).Scan(&failures); err != nil {
		return err
	}
	if failures < publishFailuresToSuspend {
		return nil
	}
	return suspendPublish(ctx, tx, id, "not_submitted", now)
}

const (
	publishFailuresToSuspend = 3
	publishLastingSuspension = 3
)

// The publish lane orders the launch's receipts by attempt ID, even when
// an own-turn receipt and a suspension are recorded in the same millisecond.
func suspendPublish(ctx context.Context, tx store.Tx, attempt int64, reason string, now time.Time) error {
	res, err := tx.ExecContext(ctx, `INSERT INTO publish_suspension (binding_id, launch_ref, reason, notice, at_ms, until_ms)
		SELECT a.binding_id, a.launch_ref, ?2, a.id, ?3,
			CASE WHEN ?2 IN ('absorbed', 'not_submitted') AND (
				SELECT COUNT(*) FROM publish_suspension s WHERE s.binding_id = a.binding_id
					AND s.launch_ref = a.launch_ref AND s.id > b.publish_resume_suspension
					AND s.reason IN ('absorbed', 'not_submitted') AND (s.until_ms IS NULL OR s.until_ms > ?3)
					AND s.notice > COALESCE((SELECT MAX(receipt.id) FROM publish_attempt receipt
						WHERE receipt.binding_id = a.binding_id AND receipt.launch_ref = a.launch_ref
							AND receipt.observation = 'own_turn'), 0)
			) + 1 >= ?5 THEN NULL ELSE ?4 END
		FROM publish_attempt a JOIN binding b ON b.id = a.binding_id WHERE a.id = ?1`,
		attempt, reason, ms(now), ms(now.Add(store.PublishSuspensionDuration)), publishLastingSuspension)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		return err
	}
	suspension, err := res.LastInsertId()
	if err != nil {
		return err
	}
	if err := recordSuspension(ctx, tx, suspension, now); err != nil {
		return err
	}
	_, err = MarkReachChanges(ctx, tx, now)
	return err
}

func RecoverPublish(ctx context.Context, tx store.Tx, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE publish_attempt SET state = 'uncertain', resolved_at_ms = ?,
		outcome_seq = (SELECT COALESCE(MAX(outcome_seq), 0) FROM publish_attempt) + id
		WHERE state = 'submitting'`, ms(now))
	return err
}

// HeldPublishQueue is a vendor queue an unobserved notice holds, and the
// adapter whose transport can prove it gone.
type HeldPublishQueue struct {
	Adapter string
	Queue   PublishQueue
}

func HeldPublishQueues(ctx context.Context, tx store.Tx) ([]HeldPublishQueue, error) {
	rows, err := tx.QueryContext(ctx, `SELECT adapter, process_pid, process_start_ms FROM publish_attempt WHERE `+openPublishLane)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var queues []HeldPublishQueue
	for rows.Next() {
		var held HeldPublishQueue
		if err := rows.Scan(&held.Adapter, &held.Queue.PID, &held.Queue.StartMS); err != nil {
			return nil, err
		}
		queues = append(queues, held)
	}
	return queues, rows.Err()
}

// RetirePublishQueue is called only after the transport proves the queue is gone.
func RetirePublishQueue(ctx context.Context, tx store.Tx, queue PublishQueue, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM publish_route_proof WHERE process_pid = ? AND process_start_ms = ?`,
		queue.PID, queue.StartMS); err != nil {
		return err
	}
	return retirePublishAttempts(ctx, tx, now, `process_pid = ? AND process_start_ms = ?`, queue.PID, queue.StartMS)
}

func retireLaunchPublish(ctx context.Context, tx store.Tx, binding, principal int64, launchRef string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM publish_route_proof WHERE binding_id = ? AND EXISTS (
		SELECT 1 FROM publish_route r WHERE r.binding_id = publish_route_proof.binding_id
		  AND r.generation = publish_route_proof.generation AND r.principal_id = ? AND r.launch_ref = ?)`,
		binding, principal, launchRef); err != nil {
		return err
	}
	return retirePublishAttempts(ctx, tx, now, `binding_id = ? AND principal_id = ? AND launch_ref = ?`, binding, principal, launchRef)
}

func retirePublishAttempts(ctx context.Context, tx store.Tx, now time.Time, scope string, args ...any) error {
	ids, err := selectIDs(ctx, tx, `SELECT id FROM publish_attempt WHERE `+scope+` AND state = 'reserved'`, args...)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := finishPublish(ctx, tx, id, "reserved", PublishNotSubmitted, now); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE publish_attempt SET retired_at_ms = ? WHERE `+scope+` AND `+
		openPublishLane, append([]any{ms(now)}, args...)...)
	return err
}
