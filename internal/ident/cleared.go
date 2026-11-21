package ident

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
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

func isCleared(ctx context.Context, tx store.Tx, id Identity) (bool, error) {
	var cleared bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM principal_key
		WHERE vendor = ? AND key = ? AND state = 'retired' AND retired_by = 'clear')`, id.Vendor, id.Key).Scan(&cleared)
	return cleared, err
}

// clearedJoin lets a conversation a /clear retired join as a new member. The
// join's own transaction releases the key and its member directory, under a
// new generation, and enrolls the new member: a join that fails releases
// nothing. After commit the directory gets the new generation and loses the
// old roster cursor, so nothing of the member it was cleared into carries
// over.
func (r SQLResolver) clearedJoin(ctx context.Context, tx store.Tx, rendezvous string, id Identity, proof Proof, fileID string, old int64) (Resolution, bool, error) {
	if cleared, err := isCleared(ctx, tx, id); err != nil || !cleared {
		return Resolution{}, false, err
	}
	clock := r.Clock
	if clock == nil {
		clock = time.Now
	}
	var generation string
	enroll := func(ctx context.Context, target store.Tx, principal int64) error {
		if target != tx {
			return errors.New("enrollment belongs to another transaction")
		}
		next := wire.NewID()
		// The directory lets go of the key before the key goes.
		result, err := tx.ExecContext(ctx, `UPDATE leaf SET state = 'live', principal_id = NULL, generation = ?
			WHERE digest = ? AND generation = ? AND file_identity = ? AND state = 'fenced' AND principal_id = ?`,
			next, proof.KeyDigest, proof.Generation, fileID, old)
		if err := exactlyOne(result, err); err != nil {
			return err
		}
		result, err = tx.ExecContext(ctx, `DELETE FROM principal_key WHERE vendor = ? AND key = ? AND retired_by = 'clear'`,
			id.Vendor, id.Key)
		if err := exactlyOne(result, err); err != nil {
			return err
		}
		result, err = tx.ExecContext(ctx, `INSERT INTO principal_key (vendor, key, principal_id, state, created_at_ms)
			SELECT ?, ?, id, 'live', ? FROM principal WHERE id = ? AND kind = 'session' AND vendor = ?`,
			id.Vendor, id.Key, clock().UnixMilli(), principal, id.Vendor)
		if err := exactlyOne(result, err); err != nil {
			return err
		}
		result, err = tx.ExecContext(ctx, `UPDATE leaf SET principal_id = ? WHERE digest = ?`, principal, proof.KeyDigest)
		if err := exactlyOne(result, err); err != nil {
			return err
		}
		generation = next
		return nil
	}
	after := func() error {
		if generation == "" {
			return nil
		}
		reg := Registration{rendezvous: rendezvous, digest: proof.KeyDigest, fileID: fileID, generation: generation}
		if err := reg.Publish(); err != nil {
			return err
		}
		members, err := openMembers(rendezvous)
		if err != nil {
			return err
		}
		defer members.Close()
		leaf, err := OpenPrivateChild(members, proof.KeyDigest[:16])
		if err != nil {
			return err
		}
		defer leaf.Close()
		if err := leaf.Remove("ack"); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	return Resolution{Caller: core.Caller{Role: core.RoleUnenrolled}, Vendor: id.Vendor, Key: id.Key,
		Enroll: enroll, AfterCommit: after}, true, nil
}
