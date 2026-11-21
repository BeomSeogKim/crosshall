package ident

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// retiredKey is the refusal for a retired key. A key a /clear retired names
// the member its conversation was cleared into; a join makes it a new one.
func retiredKey(ctx context.Context, tx store.Tx, id Identity, retired string) error {
	var alias, channel string
	err := tx.QueryRowContext(ctx, `SELECT b.alias, c.name FROM principal_key k
		JOIN binding b ON b.principal_id = k.principal_id JOIN channel c ON c.id = b.channel_id
		WHERE k.vendor = ? AND k.key = ? AND k.retired_by = 'clear' ORDER BY b.id LIMIT 1`,
		id.Vendor, id.Key).Scan(&alias, &channel)
	if errors.Is(err, sql.ErrNoRows) {
		return &EnvironmentError{core.CodeKeyRetired, retired}
	}
	if err != nil {
		return err
	}
	return &EnvironmentError{core.CodeKeyCleared, fmt.Sprintf("this conversation was cleared into @%s in #%s", alias, channel)}
}

// rejoinCleared lets a conversation a /clear retired join as a new member:
// its key and member directory are released, under a new generation, so
// nothing of the member it was cleared into carries over. Any other retired
// key, and any first contact that is not a join, stays refused.
func rejoinCleared(ctx context.Context, tx store.Tx, id Identity, intent string, now time.Time) (bool, error) {
	if intent != "join" {
		return false, nil
	}
	var cleared bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM principal_key
		WHERE vendor = ? AND key = ? AND state = 'retired' AND retired_by = 'clear')`, id.Vendor, id.Key).Scan(&cleared); err != nil || !cleared {
		return false, err
	}
	// The directory lets go of the key before the key goes.
	if _, err := tx.ExecContext(ctx, `UPDATE leaf SET state = 'live', principal_id = NULL, generation = ?, created_at_ms = ?
		WHERE vendor = ? AND key = ?`, wire.NewID(), now.UnixMilli(), id.Vendor, id.Key); err != nil {
		return false, err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM principal_key WHERE vendor = ? AND key = ?`, id.Vendor, id.Key)
	return err == nil, err
}
