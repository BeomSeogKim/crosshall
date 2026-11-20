package domain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"math/bits"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

const VerbBudgetPolicy = "owner budget policy"
const rootRefillUnits = int64(time.Hour / time.Millisecond)

type BudgetPolicy struct {
	Channel string `json:"channel"`
	Alias   string `json:"alias,omitempty"`
	Refill  *int64 `json:"refill_per_hour,omitempty"`
	Ceiling *int64 `json:"ceiling,omitempty"`
}

func (BudgetPolicy) command()     {}
func (BudgetPolicy) Verb() string { return VerbBudgetPolicy }

type BudgetPolicyResult struct {
	Channel string `json:"channel"`
	Alias   string `json:"alias,omitempty"`
	Refill  int64  `json:"refill_per_hour"`
	Ceiling int64  `json:"ceiling"`
}

type rootBudget struct {
	tokens   int64
	nextAtMS int64
	refill   int64
	ceiling  int64
}

func refillRoots(ctx context.Context, tx store.Tx, binding int64, now time.Time) (rootBudget, error) {
	var budget rootBudget
	var principal, anchor, fraction int64
	err := tx.QueryRowContext(ctx, `SELECT p.id, p.root_tokens, COALESCE(p.root_refill_at_ms, p.created_at_ms),
		p.root_refill_fraction, COALESCE(b.root_refill_per_hour, c.root_refill_per_hour),
		COALESCE(b.root_refill_ceiling, c.root_refill_ceiling)
		FROM binding b JOIN principal p ON p.id = b.principal_id JOIN channel c ON c.id = b.channel_id
		WHERE b.id = ?`, binding).Scan(&principal, &budget.tokens, &anchor, &fraction, &budget.refill, &budget.ceiling)
	refill, ceiling := budget.refill, budget.ceiling
	if err != nil || refill == 0 || ceiling == 0 {
		return budget, err
	}
	at := ms(now)
	if at > anchor || (budget.tokens >= ceiling && fraction != 0) {
		var earned int64
		if budget.tokens < ceiling {
			// One token is an hour's worth of millisecond units. Retaining the
			// remainder keeps rates whose intervals are not whole milliseconds exact.
			elapsed := uint64(at) - uint64(anchor)
			hi, lo := bits.Mul64(elapsed, uint64(refill))
			lo, carry := bits.Add64(lo, uint64(fraction), 0)
			whole, rest := bits.Div64(hi+carry, lo, uint64(rootRefillUnits))
			earned = int64(min(whole, uint64(ceiling-budget.tokens)))
			fraction = int64(rest)
		}
		anchor = max(anchor, at)
		budget.tokens += earned
		if budget.tokens >= ceiling {
			fraction = 0
		}
		if _, err := tx.ExecContext(ctx, `UPDATE principal SET root_tokens = ?, root_refill_at_ms = ?, root_refill_fraction = ? WHERE id = ?`,
			budget.tokens, anchor, fraction, principal); err != nil {
			return rootBudget{}, err
		}
		if earned != 0 {
			if _, err := tx.ExecContext(ctx, `INSERT INTO budget_event (principal_id, delta, reason, by_id, at_ms)
				VALUES (?, ?, 'roots', ?, ?)`, principal, earned, principal, at); err != nil {
				return rootBudget{}, err
			}
		}
	}
	if budget.tokens < ceiling {
		delay := (rootRefillUnits - fraction + refill - 1) / refill
		if anchor <= math.MaxInt64-delay {
			budget.nextAtMS = anchor + delay
		}
	}
	return budget, nil
}

func rootRefillTime(at int64) string {
	return time.UnixMilli(at).UTC().Format(time.RFC3339Nano)
}

func (s *Service) budgetPolicy(ctx context.Context, tx store.Tx, caller core.Caller, req Request, cmd BudgetPolicy, now time.Time) (core.Result, error) {
	if caller.Role != core.RoleOwner {
		return Refuse(core.CodeOwnerOnly, "only the owner can set root token policy", "crosshall status shows your root tokens"), nil
	}
	if !ValidName(cmd.Channel) || (cmd.Alias != "" && !ValidName(cmd.Alias)) ||
		(cmd.Refill != nil && (*cmd.Refill < 0 || *cmd.Refill > rootRefillUnits)) || (cmd.Ceiling != nil && *cmd.Ceiling < 0) {
		return Fail(core.ExitUsage, "", "invalid root token policy",
			"crosshall owner budget policy <channel>[/@<alias>] [--refill N/h] [--ceiling N]"), nil
	}
	ch, found, err := channelByName(ctx, tx, cmd.Channel)
	if err != nil {
		return core.Result{}, err
	}
	if !found {
		return Refuse(core.CodeNotFound, fmt.Sprintf("no channel named %s", cmd.Channel), "crosshall owner status --all lists the channels"), nil
	}
	change := cmd.Refill != nil || cmd.Ceiling != nil
	scope := "channel:" + ch.name
	var owner int64
	if change {
		owner, err = ownerPrincipal(ctx, tx, now)
		if err != nil {
			return core.Result{}, err
		}
		if r, found, err := replay(ctx, tx, owner, req.OpKey, scope, cmd); found || err != nil {
			return r, err
		}
	}
	before := BudgetPolicyResult{Channel: ch.name, Alias: cmd.Alias}
	var binding int64
	if cmd.Alias == "" {
		err = tx.QueryRowContext(ctx, `SELECT root_refill_per_hour, root_refill_ceiling FROM channel WHERE id = ?`, ch.id).
			Scan(&before.Refill, &before.Ceiling)
	} else {
		err = tx.QueryRowContext(ctx, `SELECT b.id, COALESCE(b.root_refill_per_hour, c.root_refill_per_hour),
			COALESCE(b.root_refill_ceiling, c.root_refill_ceiling)
			FROM binding b JOIN channel c ON c.id = b.channel_id JOIN principal p ON p.id = b.principal_id
			WHERE b.channel_id = ? AND b.alias = ? AND b.state = 'live' AND p.kind = 'session'`,
			ch.id, cmd.Alias).Scan(&binding, &before.Refill, &before.Ceiling)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return Refuse(core.CodeNotFound, fmt.Sprintf("no live member @%s in #%s", cmd.Alias, ch.name), "crosshall owner who --all"), nil
	}
	if err != nil {
		return core.Result{}, err
	}
	if !change {
		return OK(before)
	}
	after := before
	if cmd.Refill != nil {
		after.Refill = *cmd.Refill
	}
	if cmd.Ceiling != nil {
		after.Ceiling = *cmd.Ceiling
	}
	if err := applyRootPolicy(ctx, tx, ch.id, binding, cmd, before, after, now); err != nil {
		return core.Result{}, err
	}
	if err := audit(ctx, tx, "root_budget_policy", scope, owner, after, now); err != nil {
		return core.Result{}, err
	}
	r, err := OK(after)
	if err != nil {
		return core.Result{}, err
	}
	return r, record(ctx, tx, owner, req.OpKey, scope, cmd, r, now)
}

func applyRootPolicy(ctx context.Context, tx store.Tx, channel, binding int64, cmd BudgetPolicy, before, after BudgetPolicyResult, now time.Time) error {
	type previousBudget struct {
		binding int64
		budget  rootBudget
	}
	var previous []previousBudget
	if before != after {
		bindings := []int64{binding}
		if binding == 0 {
			var err error
			bindings, err = selectIDs(ctx, tx, `SELECT b.id FROM binding b JOIN principal p ON p.id = b.principal_id
				WHERE b.channel_id = ? AND p.kind = 'session' AND ((? AND b.root_refill_per_hour IS NULL)
					OR (? AND b.root_refill_ceiling IS NULL)) ORDER BY b.id`,
				channel, before.Refill != after.Refill, before.Ceiling != after.Ceiling)
			if err != nil {
				return err
			}
		}
		for _, id := range bindings {
			budget, err := refillRoots(ctx, tx, id, now)
			if err != nil {
				return err
			}
			previous = append(previous, previousBudget{binding: id, budget: budget})
		}
	}
	var err error
	if binding != 0 {
		_, err = tx.ExecContext(ctx, `UPDATE binding SET root_refill_per_hour = COALESCE(?, root_refill_per_hour),
			root_refill_ceiling = COALESCE(?, root_refill_ceiling) WHERE id = ?`, cmd.Refill, cmd.Ceiling, binding)
	} else {
		_, err = tx.ExecContext(ctx, `UPDATE channel SET root_refill_per_hour = ?, root_refill_ceiling = ? WHERE id = ?`,
			after.Refill, after.Ceiling, channel)
	}
	if err != nil {
		return err
	}
	// Keep partial credit when the same rate can accrue on both sides of the change.
	for _, old := range previous {
		if _, err := tx.ExecContext(ctx, `UPDATE principal AS p SET
			root_refill_at_ms = MAX(COALESCE(root_refill_at_ms, created_at_ms), ?), root_refill_fraction = 0
			WHERE EXISTS (SELECT 1 FROM binding b JOIN channel c ON c.id = b.channel_id
				WHERE b.id = ? AND b.principal_id = p.id AND (? OR COALESCE(b.root_refill_per_hour, c.root_refill_per_hour) <> ?
					OR p.root_tokens >= COALESCE(b.root_refill_ceiling, c.root_refill_ceiling)))`,
			ms(now), old.binding, old.budget.refill == 0 || old.budget.tokens >= old.budget.ceiling, old.budget.refill); err != nil {
			return err
		}
	}
	return nil
}
