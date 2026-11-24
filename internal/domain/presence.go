package domain

import (
	"context"
	"encoding/json"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// futureSlackMS bounds how far ahead of the dispatcher a hook's clock may be
// before its fact is stored but not projected.
const futureSlackMS = store.FactFutureSlackMS

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
	// ApprovalOpen: see ObservedFact.
	ApprovalOpen bool `json:"approval_open,omitempty"`
}

// ObservedFact is a member's effective adapter fact in absolute time.
type ObservedFact struct {
	Fact         string `json:"fact"`
	Source       string `json:"source"`
	AtMS         int64  `json:"at_ms"`
	FreshUntilMS *int64 `json:"fresh_until_ms,omitempty"`
	Verified     bool   `json:"verified"`
	// ApprovalOpen: the fact is a shown permission dialog (Claude Code; a
	// Codex request may have been approved on its own), its launch is not
	// known to have ended, and the member has run no crosshall command
	// since. Only the member's owner can answer it, so it does not age out:
	// it ends with a later fact, a later command, or its launch. No hook
	// reports an answered dialog, so a turn that goes on without a crosshall
	// command shows it until its Stop.
	ApprovalOpen bool `json:"approval_open,omitempty"`
}

// At is the fact as seen at nowMS: its age, and whether its cap has passed.
func (f ObservedFact) At(nowMS int64) Observed {
	return Observed{Fact: f.Fact, Source: f.Source, AgeS: max((nowMS-f.AtMS)/1000, 0),
		Current: f.FreshUntilMS == nil || nowMS < *f.FreshUntilMS, Verified: f.Verified, ApprovalOpen: f.ApprovalOpen}
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

// inReportedTurn follows the current prompt or permission request. A denied
// dialog can lose its Stop and an absorbed typed-ahead prompt can leave an
// unfinished turn row, so the observation's freshness still matters.
func inReportedTurn(ctx context.Context, tx store.Tx, principal int64, now time.Time) (bool, error) {
	fact, err := latestFact(ctx, tx, principal)
	return reportedTurn(fact, now), err
}

// reportedTurn: an open approval holds its turn whatever its age.
func reportedTurn(fact *ObservedFact, now time.Time) bool {
	return fact != nil && (fact.ApprovalOpen || fact.At(ms(now)).Current &&
		(fact.Fact == FactPromptSubmitted || fact.Fact == FactApprovalRequested))
}

// latestFact is the principal's effective fact: the latest, in causal
// order, among the observations of its latest launch. Nil means no adapter fact.
func latestFact(ctx context.Context, tx store.Tx, principal int64) (*ObservedFact, error) {
	f, found, err := store.ReadLatestFact(ctx, tx, principal, futureSlackMS)
	if !found || err != nil {
		return nil, err
	}
	o := ObservedFact{Fact: f.Fact, Source: adapterOf(f.Profile), AtMS: f.AtMS,
		ApprovalOpen: f.ApprovalOpen()}
	if f.FreshUntilMS.Valid {
		o.FreshUntilMS = &f.FreshUntilMS.Int64
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
