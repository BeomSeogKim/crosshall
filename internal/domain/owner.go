package domain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
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

// BudgetGrant resets a thread or a member's new-thread position.
type BudgetGrant struct {
	Channel    string `json:"channel"`
	ExchangeID int64  `json:"exchange_id,omitempty"`
	Alias      string `json:"alias,omitempty"`
	BindingID  int64  `json:"binding_id,omitempty"`
	Rounds     *int64 `json:"rounds,omitempty"`
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
	BindingID  int64  `json:"binding_id,omitempty"`
	Rounds     *int64 `json:"rounds,omitempty"`
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
		return Refuse(core.CodeOwnerOnly, "only the owner can reset rounds", "run it from the owner's own shell"), nil
	}
	if cmd.BindingID != 0 {
		return grantBindingRounds(ctx, tx, req, cmd, now)
	}
	if !ValidName(cmd.Channel) {
		return Fail(core.ExitUsage, "", "invalid channel name", "crosshall owner budget grant <channel>/x<id> --rounds <n>"), nil
	}
	scope := "channel:" + cmd.Channel
	if r, found, err := replayOwnerBudget(ctx, tx, req, scope, cmd); found || err != nil {
		return r, err
	}
	if cmd.Roots != 0 || cmd.Pool != 0 {
		return Refuse(core.CodeRetired, "this grant form is retired",
			"crosshall owner budget grant <channel>/x<id> or <channel>/@<alias> with --rounds <n>"), nil
	}

	thread := cmd.ExchangeID > 0 && cmd.Alias == ""
	member := cmd.ExchangeID == 0 && ValidName(cmd.Alias) && cmd.Units == 0
	if (!thread && !member) || cmd.Units < 0 || (cmd.Rounds != nil && (*cmd.Rounds < 0 || cmd.Units != 0)) {
		return Fail(core.ExitUsage, "", "grant non-negative rounds to one thread or one live member",
			"crosshall owner budget grant <channel>/x<id> or <channel>/@<alias> with --rounds <n>"), nil
	}

	ch, found, err := channelByName(ctx, tx, cmd.Channel)
	if err != nil {
		return core.Result{}, err
	}
	if !found {
		return Refuse(core.CodeNotFound, fmt.Sprintf("no channel named %s", cmd.Channel), "crosshall owner status --all lists the channels"), nil
	}
	rounds := int64(guardDefaultRounds)
	if cmd.Rounds != nil {
		rounds = *cmd.Rounds
	} else if cmd.Units != 0 {
		rounds = cmd.Units
	}

	var r core.Result
	if thread {
		r, err = grantThreadRounds(ctx, tx, ch, cmd.ExchangeID, rounds, now)
	} else {
		r, err = grantMemberRounds(ctx, tx, ch, cmd.Alias, rounds, now)
	}
	if err != nil || r.Status != core.ExitOK || req.OpKey == "" {
		return r, err
	}
	owner, err := ownerPrincipal(ctx, tx, now)
	if err != nil {
		return core.Result{}, err
	}
	return r, record(ctx, tx, owner, req.OpKey, scope, cmd, r, now)
}

func replayOwnerBudget(ctx context.Context, tx store.Tx, req Request, scope string, cmd Command) (core.Result, bool, error) {
	if req.OpKey == "" {
		return core.Result{}, false, nil
	}
	var owner int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM principal WHERE kind = 'owner'`).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Result{}, false, nil
	}
	if err != nil {
		return core.Result{}, false, err
	}
	return replay(ctx, tx, owner, req.OpKey, scope, cmd)
}

func grantThreadRounds(ctx context.Context, tx store.Tx, ch channelRow, exchange, rounds int64, now time.Time) (core.Result, error) {
	var stopped bool
	err := tx.QueryRowContext(ctx, `SELECT episode_open = 1 FROM exchange WHERE id = ? AND channel_id = ?`, exchange, ch.id).Scan(&stopped)
	if errors.Is(err, sql.ErrNoRows) {
		return Refuse(core.CodeNotFound, fmt.Sprintf("no exchange x%d in #%s", exchange, ch.name), "crosshall owner status --channel "+ch.name), nil
	}
	if err != nil {
		return core.Result{}, err
	}
	fence, err := guardFence(ctx, tx)
	if err != nil {
		return core.Result{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE exchange SET episode_open = 0 WHERE id = ?`, exchange); err != nil {
		return core.Result{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE message SET thread_reset_after_message_id = ?, thread_round_limit = ?
        WHERE exchange_id = ? AND kind = 'ask' AND thread_root_id IS NULL`, fence, rounds, exchange); err != nil {
		return core.Result{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM thread_position WHERE thread_root_id IN (
        SELECT id FROM message WHERE exchange_id = ? AND kind = 'ask' AND thread_root_id IS NULL)`, exchange); err != nil {
		return core.Result{}, err
	}
	if err := auditGuardReset(ctx, tx, fmt.Sprintf("exchange:%d", exchange), guardReset{ch.name, rounds, fence, stopped}, now); err != nil {
		return core.Result{}, err
	}
	return OK(BudgetGrantResult{Channel: ch.name, ExchangeID: exchange, Rounds: &rounds})
}

func grantMemberRounds(ctx context.Context, tx store.Tx, ch channelRow, alias string, rounds int64, now time.Time) (core.Result, error) {
	var principal int64
	err := tx.QueryRowContext(ctx, `SELECT principal_id FROM binding WHERE channel_id = ? AND alias = ? AND state = 'live'`,
		ch.id, alias).Scan(&principal)
	if errors.Is(err, sql.ErrNoRows) {
		return noLiveBudgetMember(ch, alias), nil
	}
	if err != nil {
		return core.Result{}, err
	}
	if err := resetMemberRounds(ctx, tx, ch.name, principal, rounds, now); err != nil {
		return core.Result{}, err
	}
	return OK(BudgetGrantResult{Channel: ch.name, Alias: alias, Rounds: &rounds})
}

// grantBindingRounds resets the member a binding names, live or departed;
// the limit stays on its principal for a later return.
func grantBindingRounds(ctx context.Context, tx store.Tx, req Request, cmd BudgetGrant, now time.Time) (core.Result, error) {
	scope := "binding:" + strconv.FormatInt(cmd.BindingID, 10)
	if r, found, err := replayOwnerBudget(ctx, tx, req, scope, cmd); found || err != nil {
		return r, err
	}
	if cmd.BindingID < 0 || cmd.Channel != "" || cmd.ExchangeID != 0 || cmd.Alias != "" || cmd.Units != 0 || cmd.Roots != 0 ||
		cmd.Pool != 0 || (cmd.Rounds != nil && *cmd.Rounds < 0) {
		return Fail(core.ExitUsage, "", "grant non-negative rounds to one binding", "crosshall owner budget grant <binding-id> --rounds <n>"), nil
	}
	var channel, alias string
	var principal int64
	err := tx.QueryRowContext(ctx, `SELECT c.name, b.alias, b.principal_id FROM binding b JOIN channel c ON c.id = b.channel_id
        WHERE b.id = ?`, cmd.BindingID).Scan(&channel, &alias, &principal)
	if errors.Is(err, sql.ErrNoRows) {
		return Refuse(core.CodeNotFound, fmt.Sprintf("no binding %d", cmd.BindingID), "crosshall owner who --all"), nil
	}
	if err != nil {
		return core.Result{}, err
	}
	rounds := int64(guardDefaultRounds)
	if cmd.Rounds != nil {
		rounds = *cmd.Rounds
	}
	if err := resetMemberRounds(ctx, tx, channel, principal, rounds, now); err != nil {
		return core.Result{}, err
	}
	r, err := OK(BudgetGrantResult{Channel: channel, Alias: alias, BindingID: cmd.BindingID, Rounds: &rounds})
	if err != nil || req.OpKey == "" {
		return r, err
	}
	owner, err := ownerPrincipal(ctx, tx, now)
	if err != nil {
		return core.Result{}, err
	}
	return r, record(ctx, tx, owner, req.OpKey, scope, cmd, r, now)
}

func resetMemberRounds(ctx context.Context, tx store.Tx, channel string, principal, rounds int64, now time.Time) error {
	var stopped bool
	if err := tx.QueryRowContext(ctx, `SELECT guard_told = 1 FROM principal WHERE id = ?`, principal).Scan(&stopped); err != nil {
		return err
	}
	fence, err := guardFence(ctx, tx)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE principal SET round_limit = ?, any_level_ms = 0, guard_told = 0, reset_after_message_id = ?
        WHERE id = ?`, rounds, fence, principal); err != nil {
		return err
	}
	return auditGuardReset(ctx, tx, fmt.Sprintf("principal:%d", principal), guardReset{channel, rounds, fence, stopped}, now)
}

func guardFence(ctx context.Context, tx store.Tx) (int64, error) {
	var fence int64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM message`).Scan(&fence)
	return fence, err
}

type guardReset struct {
	Channel             string `json:"channel"`
	Rounds              int64  `json:"rounds"`
	ResetAfterMessageID int64  `json:"reset_after_message_id"`
	WasStopped          bool   `json:"was_stopped"`
}

func auditGuardReset(ctx context.Context, tx store.Tx, subject string, detail guardReset, now time.Time) error {
	owner, err := ownerPrincipal(ctx, tx, now)
	if err != nil {
		return err
	}
	return audit(ctx, tx, guardResetKind, subject, owner, detail, now)
}

func noLiveBudgetMember(ch channelRow, alias string) core.Result {
	return Refuse(core.CodeNotFound, fmt.Sprintf("no live member @%s in #%s", alias, ch.name), "crosshall owner who --all")
}
