package dispatch

import (
	"context"
	"database/sql"
	"errors"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func matchesMembership(admitted, current *core.Membership) bool {
	if admitted == nil {
		return current == nil || current.Generation <= 1
	}
	if current == nil {
		return *admitted == (core.Membership{})
	}
	return *admitted == *current
}

func callerMembership(caller core.Caller) core.Membership {
	if caller.Membership == nil {
		return core.Membership{}
	}
	return *caller.Membership
}

func matchesReceiptMembership(generation int64, caller core.Caller) bool {
	current := callerMembership(caller).Generation
	return generation == current || generation == 0 && current == 1
}

func leafSelectedMembership(ctx context.Context, tx store.Tx, digest string) (core.Membership, error) {
	var membership core.Membership
	err := tx.QueryRowContext(ctx, `SELECT s.binding_id, s.generation FROM leaf l
		JOIN member_selection s ON s.principal_id = l.principal_id WHERE l.digest = ?`, digest).
		Scan(&membership.BindingID, &membership.Generation)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	}
	return membership, err
}

func membershipChanged(req wire.Request) core.Result {
	if req.Membership == nil {
		return domain.Fail(core.ExitRefused, core.CodeBindingChanged, "the session moved to another channel",
			"update crosshall and run the command again from the current session")
	}
	return domain.Fail(core.ExitRefused, core.CodeBindingChanged, "the session's channel changed before this command ran",
		"run the command again from the current session")
}
