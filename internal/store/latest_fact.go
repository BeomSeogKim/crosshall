package store

import (
	"context"
	"database/sql"
	"errors"
)

// LatestFact is a principal's effective adapter fact as stored.
type LatestFact struct {
	Fact         string
	Profile      string
	AtMS         int64
	FreshUntilMS sql.NullInt64
	// LaunchLive says the fact's launch has not ended and is not fenced:
	// no session end and no sign that its process is gone.
	LaunchLive bool
}

// ReadLatestFact returns the principal's effective fact: the latest, in
// causal order, among the observations of its latest launch. found is false
// when there is no adapter fact. A hook clock more than slackMS ahead of the
// dispatcher's when stored does not count.
func ReadLatestFact(ctx context.Context, tx Tx, principal, slackMS int64) (LatestFact, bool, error) {
	var f LatestFact
	// Facts count only from the latest start of the latest launch (a reused
	// session-scoped reference starts over at a resume), only when the hook's
	// clock was within the skew allowance of the dispatcher's when stored, and
	// in causal order: a later turn outranks an earlier one whatever the
	// clocks say, and within a turn the later stage wins.
	err := tx.QueryRowContext(ctx, `
		WITH latest AS (SELECT launch_ref, started_hook_at_ms, ended_hook_at_ms IS NULL AND fenced = 0 AS live
		                FROM adapter_launch WHERE principal_id = ?1
		                ORDER BY ended_hook_at_ms IS NOT NULL, started_hook_at_ms DESC, rowid DESC LIMIT 1),
		candidate AS (
			SELECT o.seq, o.kind, o.profile, o.hook_at_ms, o.fresh_until_ms, o.turn_ref, o.launch_ref,
			       CASE o.kind WHEN 'session_ended' THEN 4 WHEN 'turn_finishing' THEN 3
			                   WHEN 'approval_requested' THEN 2 ELSE 1 END AS stage,
			       COALESCE((SELECT t.opened_hook_at_ms FROM turn t WHERE t.principal_id = o.principal_id
			                 AND t.launch_ref = o.launch_ref AND t.turn_ref = o.turn_ref), o.hook_at_ms) AS position
			FROM observation o JOIN latest ON o.launch_ref = latest.launch_ref
			WHERE o.principal_id = ?1 AND o.kind NOT IN ('command', 'idle_prompt')
			  AND o.hook_at_ms <= o.observed_at_ms + ?2),
		staged AS (
			SELECT c.*, MAX(c.stage) OVER (PARTITION BY c.turn_ref) AS top FROM candidate c)
		SELECT c.kind, c.profile, c.hook_at_ms, c.fresh_until_ms, (SELECT live FROM latest) FROM staged c
		-- A turn-bound fact belongs to the start its turn opened after, whatever
		-- its own clock says; any other fact is placed by its own time. Within
		-- a turn only its latest stage counts.
		WHERE c.position >= (SELECT started_hook_at_ms FROM latest) AND (c.turn_ref IS NULL OR c.stage = c.top)
		ORDER BY c.position DESC, c.stage DESC, c.hook_at_ms DESC, c.seq DESC
		LIMIT 1`, principal, slackMS).
		Scan(&f.Fact, &f.Profile, &f.AtMS, &f.FreshUntilMS, &f.LaunchLive)
	if errors.Is(err, sql.ErrNoRows) {
		return LatestFact{}, false, nil
	}
	return f, err == nil, err
}
