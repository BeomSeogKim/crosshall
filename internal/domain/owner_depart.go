package domain

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

const VerbOwnerDepart = "owner depart"

const departureTokenBytes = 12

// OwnerDepart retires a binding selected from a particular owner listing.
type OwnerDepart struct {
	BindingID int64  `json:"binding_id"`
	Seen      string `json:"seen"`
}

func (OwnerDepart) command()     {}
func (OwnerDepart) Verb() string { return VerbOwnerDepart }

type OwnerDepartResult struct {
	BindingID int64  `json:"binding_id"`
	Channel   string `json:"channel"`
	Alias     string `json:"alias"`
}

func ValidDepartureToken(token string) bool {
	return len(token) == 2*departureTokenBytes && strings.Trim(token, "0123456789abcdef") == ""
}

func departureToken(ctx context.Context, tx store.Tx, binding, principal int64) (string, error) {
	var observation, hook, roster, identity int64
	// Use committed row order instead of timestamps so activity at the same
	// clock tick still invalidates the listing.
	err := tx.QueryRowContext(ctx, `SELECT
		COALESCE((SELECT MAX(seq) FROM observation WHERE principal_id = ?2), 0),
		COALESCE((SELECT MAX(rowid) FROM op WHERE principal_id = ?2 AND scope = 'hook'), 0),
		COALESCE((SELECT MAX(rc.seq) FROM roster_change rc JOIN binding b ON b.channel_id = rc.channel_id
			WHERE b.id = ?1 AND rc.principal_id = ?2), 0),
		COALESCE((SELECT MAX(rowid) FROM principal_key WHERE principal_id = ?2 AND state = 'live'), 0)`,
		binding, principal).Scan(&observation, &hook, &roster, &identity)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(fmt.Appendf(nil, "%d/%d/%d/%d/%d/%d", binding, principal, observation, hook, roster, identity))
	return hex.EncodeToString(sum[:departureTokenBytes]), nil
}

func (s *Service) ownerDepart(ctx context.Context, tx store.Tx, caller core.Caller, req Request, cmd OwnerDepart, now time.Time) (core.Result, error) {
	const list = "crosshall owner who --all"
	if caller.Role != core.RoleOwner {
		return Refuse(core.CodeOwnerOnly, "only the owner can depart a binding", "run it from the owner's own shell, not an agent session"), nil
	}
	if cmd.BindingID <= 0 || !ValidDepartureToken(cmd.Seen) || req.OpKey == "" {
		return Fail(core.ExitUsage, "", "depart requires a binding ID and its listing token", list), nil
	}
	owner, err := ownerPrincipal(ctx, tx, now)
	if err != nil {
		return core.Result{}, err
	}
	scope := "binding:" + strconv.FormatInt(cmd.BindingID, 10)
	if result, found, err := replay(ctx, tx, owner, req.OpKey, scope, cmd); found || err != nil {
		return result, err
	}
	out := OwnerDepartResult{BindingID: cmd.BindingID}
	var channel, principal int64
	var state, vendor string
	err = tx.QueryRowContext(ctx, `SELECT b.channel_id, b.principal_id, c.name, b.alias, b.state, p.vendor
		FROM binding b JOIN channel c ON c.id = b.channel_id JOIN principal p ON p.id = b.principal_id
		WHERE b.id = ?`, cmd.BindingID).Scan(&channel, &principal, &out.Channel, &out.Alias, &state, &vendor)
	if errors.Is(err, sql.ErrNoRows) {
		return Refuse(core.CodeNotFound, fmt.Sprintf("no binding with ID %d", cmd.BindingID), list), nil
	}
	if err != nil {
		return core.Result{}, err
	}
	if state != "live" {
		return Refuse(core.CodeDeparted, "the binding already departed", list), nil
	}
	token, err := departureToken(ctx, tx, cmd.BindingID, principal)
	if err != nil {
		return core.Result{}, err
	}
	if token != cmd.Seen {
		return Refuse(core.CodeBindingChanged, "the binding changed since it was listed", list), nil
	}

	if _, err := tx.ExecContext(ctx, `UPDATE binding SET state = 'departed', departed_at_ms = ?,
		departed_by_hook = 0, departed_by_owner = 1, owner_hook_fence_ms = MAX(?, COALESCE(owner_hook_fence_ms, 0),
			COALESCE((SELECT MAX(started_hook_at_ms) FROM adapter_launch WHERE principal_id = ?), 0))
		WHERE id = ?`, ms(now), ms(now), principal, cmd.BindingID); err != nil {
		return core.Result{}, err
	}
	if err := rosterChange(ctx, tx, channel, principal, "departed"); err != nil {
		return core.Result{}, err
	}
	if ValidAdapterName(vendor) {
		if _, err := tx.ExecContext(ctx, `INSERT INTO adapter_dirty(principal_id, notice, reminder) VALUES (?, 1, 1)
			ON CONFLICT(principal_id) DO UPDATE SET notice = 1, reminder = 1`, principal); err != nil {
			return core.Result{}, err
		}
		if err := refreshMemberAdapterCapabilities(ctx, tx, principal, vendor); err != nil {
			return core.Result{}, err
		}
	}
	if err := audit(ctx, tx, "binding_departed", scope, owner, out, now); err != nil {
		return core.Result{}, err
	}
	result, err := OK(out)
	if err != nil {
		return core.Result{}, err
	}
	return result, record(ctx, tx, owner, req.OpKey, scope, cmd, result, now)
}
