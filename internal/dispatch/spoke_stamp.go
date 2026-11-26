package dispatch

import (
	"context"
	"database/sql"
	"errors"
	"os"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// stampLeaf publishes a session's membership stamp on a linked device
// (#400): under its leaf's publish lock, from a read of the registration
// taken under that lock, so no writer puts back a selection it read
// before another writer's change, and a hook that publishes after the
// unlock reads the stamp the store holds.
func stampLeaf(ctx context.Context, db *store.Store, rendezvous, vendor, key string) error {
	var digest, identity string
	err := db.View(ctx, func(tx store.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT digest, file_identity FROM spoke_leaf WHERE vendor = ? AND key = ?`,
			vendor, key).Scan(&digest, &identity)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	dir, err := ident.OpenSpokeLeaf(rendezvous, digest)
	if errors.Is(err, os.ErrNotExist) {
		return nil // first contact registers it again
	}
	if err != nil {
		return err
	}
	defer dir.Close()
	_, unlock, err := spool.LockLeafPublish(ctx, dir)
	if err != nil {
		return err
	}
	defer unlock()
	return stampLocked(ctx, db, dir, digest, identity)
}

// stampLocked writes the stamp of the registration (digest, identity) into
// dir, whose publish lock the caller holds. A directory or registration
// that changed meanwhile is left alone.
func stampLocked(ctx context.Context, db *store.Store, dir *os.Root, digest, identity string) error {
	if got, err := ident.FileIdentity(dir); err != nil || got != identity {
		return err
	}
	var m core.Membership
	err := db.View(ctx, func(tx store.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT binding_id, membership_gen FROM spoke_leaf WHERE digest = ? AND file_identity = ?`,
			digest, identity).Scan(&m.BindingID, &m.Generation)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if m.Generation == 0 {
		return ident.WithdrawMembership(dir) // the hall holds no selection
	}
	return ident.RepublishMembership(dir, m)
}
