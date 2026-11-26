package dispatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"

	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/link"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// stageSpokeWakeEvent runs inside the writer that advances the event cursor.
func stageSpokeWakeEvent(ctx context.Context, tx store.Tx, event link.Event) error {
	switch event.Kind {
	case link.EventWake:
		var wake link.Wake
		if err := json.Unmarshal(event.Body, &wake); err != nil {
			return err
		}
		return stageSpokeWake(ctx, tx, wake)
	case link.EventWakeClosed:
		var closed link.WakeClosed
		if err := json.Unmarshal(event.Body, &closed); err != nil {
			return err
		}
		return closeSpokeWake(ctx, tx, closed.AttemptID)
	default:
		return errors.New("link: unexpected wake event")
	}
}

// stageSpokeWake also accepts the same authorization on WakeReplay. A
// reconnect may refresh a dormant row but never recreates a claim.
func stageSpokeWake(ctx context.Context, tx store.Tx, wake link.Wake) error {
	route := wake.Route
	if wake.AttemptID <= 0 || route.Vendor == "" || route.Key == "" ||
		route.Boot == "" || route.Launch == "" || route.PolicyEpoch <= 0 ||
		route.Scope.LeafGeneration == "" || route.Scope.RouteEpoch <= 0 ||
		route.Scope.Membership.BindingID <= 0 || route.Scope.Membership.Generation <= 0 ||
		!link.ValidNonce(route.Lane) || len(wake.IDs) == 0 || len(wake.IDs) > domain.OfferCap {
		return errors.New("link: invalid wake authorization")
	}
	for i, id := range wake.IDs {
		if id <= 0 || i > 0 && id <= wake.IDs[i-1] {
			return errors.New("link: invalid wake message order")
		}
	}
	raw, err := json.Marshal(wake)
	if err != nil {
		return err
	}
	var stored sql.NullString
	var state string
	err = tx.QueryRowContext(ctx, `SELECT wake, state FROM spoke_wake_attempt WHERE attempt_id = ?`, wake.AttemptID).
		Scan(&stored, &state)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.ExecContext(ctx, `INSERT INTO spoke_wake_attempt (attempt_id, wake, state) VALUES (?, ?, 'staged')`,
			wake.AttemptID, string(raw))
		return err
	}
	if err != nil {
		return err
	}
	if stored.Valid && stored.String != string(raw) {
		return errors.New("link: wake attempt changed its authorization")
	}
	if state == "dormant" {
		_, err = tx.ExecContext(ctx, `UPDATE spoke_wake_attempt SET state = 'staged' WHERE attempt_id = ?`, wake.AttemptID)
	}
	return err
}

func closeSpokeWake(ctx context.Context, tx store.Tx, attempt int64) error {
	if attempt <= 0 {
		return errors.New("link: invalid closed wake")
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO spoke_wake_attempt (attempt_id, state) VALUES (?, 'closed')
		ON CONFLICT (attempt_id) DO UPDATE SET state = 'closed' WHERE state IN ('staged', 'dormant')`, attempt)
	return err
}

func suspendSpokeWakes(ctx context.Context, tx store.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE spoke_wake_attempt SET state = 'dormant' WHERE state = 'staged'`)
	return err
}

// claimSpokeWake records the send boundary. Its caller owns the current
// stream and leaf lock, and supplies a freshly verified local route and cut.
// The writer must commit before the returned wake can reach the sender.
func claimSpokeWake(ctx context.Context, tx store.Tx, attempt int64, current link.WakeRoute, cut uint64) (link.Wake, bool, error) {
	if cut == 0 || cut > math.MaxInt64 {
		return link.Wake{}, false, errors.New("link: invalid wake submission cut")
	}
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT wake FROM spoke_wake_attempt WHERE attempt_id = ? AND state = 'staged'`, attempt).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return link.Wake{}, false, nil
	}
	if err != nil {
		return link.Wake{}, false, err
	}
	var wake link.Wake
	if err := json.Unmarshal([]byte(raw), &wake); err != nil {
		return link.Wake{}, false, err
	}
	if wake.Route != current {
		return link.Wake{}, false, nil
	}
	res, err := tx.ExecContext(ctx, `UPDATE spoke_wake_attempt SET state = 'claimed', submission_seq = ?
		WHERE attempt_id = ? AND state = 'staged'`, cut, attempt)
	if err != nil {
		return link.Wake{}, false, err
	}
	n, err := res.RowsAffected()
	return wake, n == 1, err
}

// finishSpokeWake may commit a negative before the claim when the caller
// can prove no invocation can occur. Other outcomes need a stored cut.
func finishSpokeWake(ctx context.Context, tx store.Tx, attempt int64, outcome string) error {
	switch outcome {
	case link.WakeNotSubmitted, link.WakeSubmitted, link.WakeUncertain:
	default:
		return errors.New("link: invalid wake outcome")
	}
	_, err := tx.ExecContext(ctx, `UPDATE spoke_wake_attempt SET state = 'finished', outcome = ?1
		WHERE attempt_id = ?2 AND (state = 'claimed' OR (?1 = 'not_submitted' AND state IN ('staged', 'dormant')))`,
		outcome, attempt)
	return err
}

// Recover abandoned claims once at dispatcher start, before any sender can
// run. A reconnect alone does not turn a live worker's claim uncertain.
func recoverSpokeWakeClaims(ctx context.Context, tx store.Tx) error {
	_, err := tx.ExecContext(ctx, `UPDATE spoke_wake_attempt SET state = 'finished', outcome = 'uncertain' WHERE state = 'claimed'`)
	return err
}

func nextSpokeWakeResult(ctx context.Context, tx store.Tx) (*link.WakeResult, error) {
	var result link.WakeResult
	var raw string
	err := tx.QueryRowContext(ctx, `SELECT attempt_id, wake, outcome, submission_seq FROM spoke_wake_attempt
		WHERE state = 'finished' AND recorded = 0 ORDER BY attempt_id LIMIT 1`).
		Scan(&result.AttemptID, &raw, &result.Outcome, &result.SubmissionSeq)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var wake link.Wake
	if err := json.Unmarshal([]byte(raw), &wake); err != nil {
		return nil, err
	}
	result.Route = wake.Route
	return &result, nil
}

func recordSpokeWakeReceipt(ctx context.Context, tx store.Tx, sending int64, result link.WakeRecorded) error {
	if sending <= 0 || result.AttemptID != sending ||
		(result.Status != link.RecordedApplied && result.Status != link.RecordedStale) {
		return errors.New("link: wake receipt did not match the pending result")
	}
	res, err := tx.ExecContext(ctx, `UPDATE spoke_wake_attempt SET recorded = 1
		WHERE attempt_id = ? AND state = 'finished' AND recorded = 0`, sending)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err == nil && n != 1 {
		return errors.New("link: pending wake result is missing")
	}
	return err
}
