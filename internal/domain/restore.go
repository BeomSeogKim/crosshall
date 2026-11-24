package domain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

const VerbRestore = "owner restore"

// Restore replaces the session identity for one binding's principal.
type Restore struct {
	BindingID int64  `json:"binding_id"`
	Vendor    string `json:"vendor"`
	Key       string `json:"key"`
}

func (Restore) command()     {}
func (Restore) Verb() string { return VerbRestore }

type RestoreResult struct {
	BindingID int64 `json:"binding_id"`
}

func (s *Service) restore(ctx context.Context, tx store.Tx, caller core.Caller, req Request, cmd Restore, now time.Time) (core.Result, error) {
	if caller.Role != core.RoleOwner {
		return Refuse(core.CodeOwnerOnly, "only the owner can restore a binding", "run it from the owner's own shell, not an agent session"), nil
	}
	id, err := ident.ParseRestoreKey(cmd.Vendor, cmd.Key)
	if err != nil || cmd.BindingID <= 0 {
		return Fail(core.ExitUsage, "", "restore requires a positive binding ID and a valid replacement identity", "get the key with crosshall whoami --full"), nil
	}
	cmd.Key = id.Key // Route keys have a canonical lowercase representation.
	owner, err := ownerPrincipal(ctx, tx, now)
	if err != nil {
		return core.Result{}, err
	}
	const scope = "owner"
	if r, found, err := replay(ctx, tx, owner, req.OpKey, scope, cmd); err != nil || found {
		return r, err
	}
	var principal, selected int64
	err = tx.QueryRowContext(ctx, `SELECT b.principal_id, COALESCE(s.binding_id, 0) FROM binding b
		LEFT JOIN member_selection s ON s.principal_id = b.principal_id WHERE b.id = ?`, cmd.BindingID).Scan(&principal, &selected)
	if errors.Is(err, sql.ErrNoRows) {
		return Refuse(core.CodeNotFound, fmt.Sprintf("no binding with ID %d", cmd.BindingID), "crosshall owner who --all"), nil
	}
	if err != nil {
		return core.Result{}, err
	}
	if selected != cmd.BindingID {
		return Refuse(core.CodeBindingChanged, fmt.Sprintf("binding %d is historical", cmd.BindingID),
			fmt.Sprintf("retry with crosshall owner restore %d and the replacement identity", selected)), nil
	}
	if s.RestoreKey == nil {
		return core.Result{}, errors.New("domain: missing identity restore handler")
	}
	attached, err := s.RestoreKey(ctx, tx, principal, id.Vendor, id.Key, now)
	if err != nil {
		return core.Result{}, err
	}
	if !attached {
		return Refuse(core.CodeKeyInUse, "that replacement identity was already enrolled", "choose a session that has never joined"), nil
	}
	out := RestoreResult{BindingID: cmd.BindingID}
	if err := handOverBinding(ctx, tx, cmd.BindingID, principal, owner, out, now); err != nil {
		return core.Result{}, err
	}
	r, err := OK(out)
	if err != nil {
		return core.Result{}, err
	}
	return r, record(ctx, tx, owner, req.OpKey, scope, cmd, r, now)
}
