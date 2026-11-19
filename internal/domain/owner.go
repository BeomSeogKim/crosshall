package domain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

const (
	VerbCancel      = "owner cancel"
	VerbBudgetGrant = "owner budget grant"
)

// Cancel closes the outstanding obligations of an ask in the named channel.
type Cancel struct {
	Channel string `json:"channel"`
	ID      int64  `json:"id"`
}

func (Cancel) command()     {}
func (Cancel) Verb() string { return VerbCancel }

// BudgetGrant adds units to an exchange or root tokens to a channel member.
type BudgetGrant struct {
	Channel    string `json:"channel"`
	ExchangeID int64  `json:"exchange_id,omitempty"`
	Alias      string `json:"alias,omitempty"`
	Units      int64  `json:"units,omitempty"`
	Roots      int64  `json:"roots,omitempty"`
	Pool       int64  `json:"pool,omitempty"`
}

func (BudgetGrant) command()     {}
func (BudgetGrant) Verb() string { return VerbBudgetGrant }

type CancelResult struct {
	Channel   string `json:"channel"`
	ID        int64  `json:"id"`
	Cancelled int64  `json:"cancelled"`
}

type BudgetGrantResult struct {
	Channel    string `json:"channel"`
	ExchangeID int64  `json:"exchange_id,omitempty"`
	Alias      string `json:"alias,omitempty"`
	Units      int64  `json:"units,omitempty"`
	Roots      int64  `json:"roots,omitempty"`
	Remaining  int64  `json:"remaining,omitempty"`
	RootTokens int64  `json:"root_tokens,omitempty"`
	Pool       int64  `json:"pool,omitempty"`
	JoinPool   int64  `json:"join_pool,omitempty"`
}

func (s *Service) cancel(ctx context.Context, tx store.Tx, caller core.Caller, req Request, cmd Cancel, now time.Time) (core.Result, error) {
	if caller.Role != core.RoleOwner {
		return Refuse(core.CodeOwnerOnly, "only the owner can cancel requests", "run it from the owner's own shell, not an agent session"), nil
	}
	if !ValidName(cmd.Channel) || cmd.ID < 1 {
		return Fail(core.ExitUsage, "", "cancel requires a channel and a positive request ID", "crosshall owner cancel <channel>/<id>"), nil
	}
	ch, found, err := channelByName(ctx, tx, cmd.Channel)
	if err != nil {
		return core.Result{}, err
	}
	if !found {
		return Refuse(core.CodeNotFound, fmt.Sprintf("no channel named %s", cmd.Channel), "crosshall owner status --all lists the channels"), nil
	}
	owner, err := ownerPrincipal(ctx, tx, now)
	if err != nil {
		return core.Result{}, err
	}
	scope := "channel:" + ch.name
	if r, found, err := replay(ctx, tx, owner, req.OpKey, scope, cmd); found || err != nil {
		return r, err
	}
	var askID int64
	if err := tx.QueryRowContext(ctx, `SELECT id FROM message WHERE id = ? AND channel_id = ? AND kind = 'ask'`,
		cmd.ID, ch.id).Scan(&askID); errors.Is(err, sql.ErrNoRows) {
		return Refuse(core.CodeNotFound, fmt.Sprintf("no request %d in #%s", cmd.ID, ch.name), "crosshall owner status --channel "+ch.name), nil
	} else if err != nil {
		return core.Result{}, err
	}

	// The receipt insert and settlement use the same still-open recipients.
	if _, err := tx.ExecContext(ctx, `INSERT INTO receipt
		(message_id, principal_id, stage, seq, at_ms, evidence)
		SELECT message_id, principal_id, 'settled', 1, ?, 'owner'
		FROM obligation WHERE message_id = ? AND state = 'open'`, ms(now), askID); err != nil {
		return core.Result{}, err
	}
	updated, err := tx.ExecContext(ctx, `UPDATE obligation SET state = 'cancelled', settled_at_ms = ?, settled_by_id = ?
		WHERE message_id = ? AND state = 'open'`, ms(now), owner, askID)
	if err != nil {
		return core.Result{}, err
	}
	count, err := updated.RowsAffected()
	if err != nil {
		return core.Result{}, err
	}
	result := CancelResult{Channel: ch.name, ID: askID, Cancelled: count}
	if count != 0 {
		if err := audit(ctx, tx, "request_cancelled", fmt.Sprintf("message:%d", askID), owner, result, now); err != nil {
			return core.Result{}, err
		}
	}
	r, err := OK(result)
	if err != nil {
		return core.Result{}, err
	}
	return r, record(ctx, tx, owner, req.OpKey, scope, cmd, r, now)
}

func (s *Service) grantBudget(ctx context.Context, tx store.Tx, caller core.Caller, req Request, cmd BudgetGrant, now time.Time) (core.Result, error) {
	if caller.Role != core.RoleOwner {
		return Refuse(core.CodeOwnerOnly, "only the owner can grant credit", "run it from the owner's own shell, not an agent session"), nil
	}
	exchange := cmd.ExchangeID > 0 && cmd.Units > 0 && cmd.Alias == "" && cmd.Roots == 0 && cmd.Pool == 0
	member := cmd.ExchangeID == 0 && cmd.Units == 0 && ValidName(cmd.Alias) && cmd.Roots > 0 && cmd.Pool == 0
	joinPool := cmd.ExchangeID == 0 && cmd.Units == 0 && cmd.Alias == "" && cmd.Roots == 0 && cmd.Pool > 0
	if !ValidName(cmd.Channel) || (!exchange && !member && !joinPool) {
		return Fail(core.ExitUsage, "", "grant positive units to one exchange, positive roots to one member, or positive tokens to a channel's join pool",
			"crosshall owner budget grant <channel>/x<id> <units>, <channel>/@<alias> --roots <n>, or <channel> --pool <n>"), nil
	}
	ch, found, err := channelByName(ctx, tx, cmd.Channel)
	if err != nil {
		return core.Result{}, err
	}
	if !found {
		return Refuse(core.CodeNotFound, fmt.Sprintf("no channel named %s", cmd.Channel), "crosshall owner status --all lists the channels"), nil
	}
	owner, err := ownerPrincipal(ctx, tx, now)
	if err != nil {
		return core.Result{}, err
	}
	scope := "channel:" + ch.name
	if r, found, err := replay(ctx, tx, owner, req.OpKey, scope, cmd); found || err != nil {
		return r, err
	}
	var r core.Result
	switch {
	case exchange:
		r, err = grantUnits(ctx, tx, ch, cmd, owner, now)
	case joinPool:
		r, err = grantJoinPool(ctx, tx, ch, cmd, owner, now)
	default:
		r, err = grantRoots(ctx, tx, ch, cmd, owner, now)
	}
	if err != nil || r.Status != core.ExitOK {
		return r, err
	}
	return r, record(ctx, tx, owner, req.OpKey, scope, cmd, r, now)
}

func grantUnits(ctx context.Context, tx store.Tx, ch channelRow, cmd BudgetGrant, owner int64, now time.Time) (core.Result, error) {
	var granted, spent int64
	err := tx.QueryRowContext(ctx, `SELECT granted, spent FROM exchange WHERE id = ? AND channel_id = ?`,
		cmd.ExchangeID, ch.id).Scan(&granted, &spent)
	if errors.Is(err, sql.ErrNoRows) {
		return Refuse(core.CodeNotFound, fmt.Sprintf("no exchange x%d in #%s", cmd.ExchangeID, ch.name), "crosshall owner status --channel "+ch.name), nil
	}
	if err != nil {
		return core.Result{}, err
	}
	if granted > math.MaxInt64-cmd.Units {
		return Fail(core.ExitUsage, "", "grant exceeds the exchange credit limit", "grant fewer units"), nil
	}
	granted += cmd.Units
	if _, err := tx.ExecContext(ctx, `UPDATE exchange SET granted = ?, episode_open = 0 WHERE id = ?`,
		granted, cmd.ExchangeID); err != nil {
		return core.Result{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO budget_event (exchange_id, delta, reason, by_id, at_ms)
		VALUES (?, ?, 'grant', ?, ?)`, cmd.ExchangeID, cmd.Units, owner, ms(now)); err != nil {
		return core.Result{}, err
	}
	return OK(BudgetGrantResult{Channel: ch.name, ExchangeID: cmd.ExchangeID, Units: cmd.Units, Remaining: granted - spent})
}

func grantRoots(ctx context.Context, tx store.Tx, ch channelRow, cmd BudgetGrant, owner int64, now time.Time) (core.Result, error) {
	var principal, roots int64
	err := tx.QueryRowContext(ctx, `SELECT p.id, p.root_tokens FROM binding b
		JOIN principal p ON p.id = b.principal_id
		WHERE b.channel_id = ? AND b.alias = ? AND b.state = 'live' AND p.kind = 'session'`,
		ch.id, cmd.Alias).Scan(&principal, &roots)
	if errors.Is(err, sql.ErrNoRows) {
		return Refuse(core.CodeNotFound, fmt.Sprintf("no live member @%s in #%s", cmd.Alias, ch.name), "crosshall owner who --all"), nil
	}
	if err != nil {
		return core.Result{}, err
	}
	if roots > math.MaxInt64-cmd.Roots {
		return Fail(core.ExitUsage, "", "grant exceeds the member root token limit", "grant fewer root tokens"), nil
	}
	roots += cmd.Roots
	if _, err := tx.ExecContext(ctx, `UPDATE principal SET root_tokens = ? WHERE id = ?`, roots, principal); err != nil {
		return core.Result{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO budget_event (principal_id, delta, reason, by_id, at_ms)
		VALUES (?, ?, 'roots', ?, ?)`, principal, cmd.Roots, owner, ms(now)); err != nil {
		return core.Result{}, err
	}
	return OK(BudgetGrantResult{Channel: ch.name, Alias: cmd.Alias, Roots: cmd.Roots, RootTokens: roots})
}

// grantJoinPool adds root tokens for the channel's future first joins.
func grantJoinPool(ctx context.Context, tx store.Tx, ch channelRow, cmd BudgetGrant, owner int64, now time.Time) (core.Result, error) {
	var pool int64
	if err := tx.QueryRowContext(ctx, `SELECT join_pool FROM channel WHERE id = ?`, ch.id).Scan(&pool); err != nil {
		return core.Result{}, err
	}
	if pool > math.MaxInt64-cmd.Pool {
		return Fail(core.ExitUsage, "", "grant exceeds the join pool limit", "grant fewer tokens"), nil
	}
	pool += cmd.Pool
	if _, err := tx.ExecContext(ctx, `UPDATE channel SET join_pool = ? WHERE id = ?`, pool, ch.id); err != nil {
		return core.Result{}, err
	}
	result := BudgetGrantResult{Channel: ch.name, Pool: cmd.Pool, JoinPool: pool}
	if err := audit(ctx, tx, "join_pool_granted", "channel:"+ch.name, owner, result, now); err != nil {
		return core.Result{}, err
	}
	return OK(result)
}

// drawJoinToken gives a principal's first binding in a channel one root token
// from the channel's join pool, while the pool lasts. The pool is the owner's
// grant made in advance, so the event records the owner as the grantor.
func drawJoinToken(ctx context.Context, tx store.Tx, channel, principal int64, now time.Time) error {
	res, err := tx.ExecContext(ctx, `UPDATE channel SET join_pool = join_pool - 1 WHERE id = ? AND join_pool > 0`, channel)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return err
	}
	owner, err := ownerPrincipal(ctx, tx, now)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE principal SET root_tokens = root_tokens + 1 WHERE id = ?`, principal); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO budget_event (principal_id, delta, reason, by_id, at_ms)
		VALUES (?, 1, 'roots', ?, ?)`, principal, owner, ms(now))
	return err
}
