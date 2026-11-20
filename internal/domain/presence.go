package domain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// futureSlackMS bounds how far ahead of the dispatcher a hook's clock may be
// before its fact is stored but not projected.
const futureSlackMS = 5_000

// Observed is the latest literal fact an adapter reported for a member, with
// its age. Current is false once the fact's cap has passed: the state is then
// unknown and the fact is shown as the last one seen.
type Observed struct {
	Fact    string `json:"fact"`
	Source  string `json:"source"`
	AgeS    int64  `json:"age_s"`
	Current bool   `json:"current"`
	// Verified would mean the launch's vendor version and mode were
	// established. No source for them exists yet, so every
	// adapter fact is labelled unverified; launch_verified is a different
	// property (a bound launch reference) and does not set it.
	Verified bool `json:"verified"`
}

// ObservedFact is a member's effective adapter fact in absolute time.
type ObservedFact struct {
	Fact         string `json:"fact"`
	Source       string `json:"source"`
	AtMS         int64  `json:"at_ms"`
	FreshUntilMS *int64 `json:"fresh_until_ms,omitempty"`
	Verified     bool   `json:"verified"`
}

// At is the fact as seen at nowMS: its age, and whether its cap has passed.
func (f ObservedFact) At(nowMS int64) Observed {
	return Observed{Fact: f.Fact, Source: f.Source, AgeS: max((nowMS-f.AtMS)/1000, 0),
		Current: f.FreshUntilMS == nil || nowMS < *f.FreshUntilMS, Verified: f.Verified}
}

// presenceOf projects the principal's effective fact as of now. Nil means
// no adapter fact.
func presenceOf(ctx context.Context, tx store.Tx, principal int64, now time.Time) (*Observed, error) {
	f, err := latestFact(ctx, tx, principal)
	if f == nil || err != nil {
		return nil, err
	}
	o := f.At(ms(now))
	return &o, nil
}

// latestFact is the principal's effective fact: the latest, in causal
// order, among the observations of its latest launch. Nil means no adapter fact.
func latestFact(ctx context.Context, tx store.Tx, principal int64) (*ObservedFact, error) {
	var (
		o       ObservedFact
		fresh   sql.NullInt64
		adapter string
	)
	// Facts count only from the latest start of the latest launch (a reused
	// session-scoped reference starts over at a resume), only when the hook's
	// clock was within the skew allowance of the dispatcher's when stored, and
	// in causal order: a later turn outranks an earlier one whatever the
	// clocks say, and within a turn the later stage wins.
	err := tx.QueryRowContext(ctx, `
		WITH latest AS (SELECT launch_ref, started_hook_at_ms FROM adapter_launch WHERE principal_id = ?1
		                ORDER BY ended_hook_at_ms IS NOT NULL, started_hook_at_ms DESC, rowid DESC LIMIT 1),
		candidate AS (
			SELECT o.seq, o.kind, o.profile, o.hook_at_ms, o.fresh_until_ms, o.turn_ref, o.launch_ref,
			       CASE o.kind WHEN 'session_ended' THEN 4 WHEN 'turn_finishing' THEN 3
			                   WHEN 'approval_requested' THEN 2 ELSE 1 END AS stage,
			       COALESCE((SELECT t.opened_hook_at_ms FROM turn t WHERE t.principal_id = o.principal_id
			                 AND t.launch_ref = o.launch_ref AND t.turn_ref = o.turn_ref), o.hook_at_ms) AS position
			FROM observation o JOIN latest ON o.launch_ref = latest.launch_ref
			WHERE o.principal_id = ?1 AND o.kind <> 'command'
			  AND o.hook_at_ms <= o.observed_at_ms + ?2)
		SELECT c.kind, c.profile, c.hook_at_ms, c.fresh_until_ms FROM candidate c
		-- A turn-bound fact belongs to the start its turn opened after, whatever
		-- its own clock says; any other fact is placed by its own time.
		WHERE c.position >= (SELECT started_hook_at_ms FROM latest) AND NOT EXISTS (SELECT 1 FROM candidate s
		                  WHERE c.turn_ref IS NOT NULL AND s.turn_ref = c.turn_ref AND s.stage > c.stage)
		ORDER BY c.position DESC, c.stage DESC, c.hook_at_ms DESC, c.seq DESC
		LIMIT 1`, principal, futureSlackMS).
		Scan(&o.Fact, &adapter, &o.AtMS, &fresh)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	o.Source = adapterOf(adapter)
	if fresh.Valid {
		o.FreshUntilMS = &fresh.Int64
	}
	return &o, nil
}

// adapterOf takes the adapter name from a stored JSON profile.
func adapterOf(profile string) string {
	var p wire.HookProfile
	_ = json.Unmarshal([]byte(profile), &p)
	return p.Adapter
}

// presenceChanged writes one roster change when an event changed the
// member's effective fact. Age and expiry alone change nothing.
func presenceChanged(ctx context.Context, tx store.Tx, channel, principal int64, before *Observed, now time.Time) error {
	after, err := presenceOf(ctx, tx, principal, now)
	if err != nil {
		return err
	}
	// The effective fact is the fact and whether it is current: an expired
	// prompt refreshed by a new one is a change, the passing of time alone is not.
	if before == nil && after == nil || before != nil && after != nil &&
		before.Fact == after.Fact && before.Current == after.Current {
		return nil
	}
	return rosterChange(ctx, tx, channel, principal, "presence")
}

// withPresence fills the observed fact of roster rows.
func withPresence(ctx context.Context, tx store.Tx, principal int64, m *Member, now time.Time) error {
	o, err := presenceOf(ctx, tx, principal, now)
	m.Observed = o
	return err
}
