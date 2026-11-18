package ident

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

// ParseRestoreKey accepts a full session identity for an owner restore.
func ParseRestoreKey(vendor, key string) (Identity, error) {
	if digest, ok := strings.CutPrefix(key, "sha256:"); ok && len(digest) == 12 && strings.Trim(digest, "0123456789abcdef") == "" {
		return Identity{}, errors.New("get the full replacement key with crosshall whoami --full")
	}
	return parseVendorKey(vendor, key)
}

// RestoreKey attaches a never-enrolled key to principal and retires the keys it
// replaces. A prior enrollment, including a retired one, returns false.
func RestoreKey(ctx context.Context, tx store.Tx, principal int64, vendor, key string, now time.Time) (bool, error) {
	id, err := ParseRestoreKey(vendor, key)
	if err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO principal_key(vendor,key,principal_id,created_at_ms)
		VALUES(?,?,?,?) ON CONFLICT(vendor,key) DO NOTHING`, id.Vendor, id.Key, principal, now.UnixMilli())
	if err != nil {
		return false, err
	}
	inserted, err := res.RowsAffected()
	if err != nil || inserted == 0 {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE principal_key SET state='retired', retired_at_ms=?
		WHERE principal_id=? AND state='live' AND (vendor,key)<>(?,?)`, now.UnixMilli(), principal, id.Vendor, id.Key); err != nil {
		return false, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE leaf SET state='fenced' WHERE state='live' AND (vendor,key) IN
		(SELECT vendor,key FROM principal_key WHERE principal_id=? AND state='retired')`, principal); err != nil {
		return false, err
	}
	// A first contact can already have registered the new leaf without enrolling it.
	if _, err = tx.ExecContext(ctx, `UPDATE leaf SET principal_id=? WHERE vendor=? AND key=? AND principal_id IS NULL`,
		principal, id.Vendor, id.Key); err != nil {
		return false, err
	}
	// The old key's CLI version does not describe the replacement conversation.
	_, err = tx.ExecContext(ctx, "UPDATE principal SET vendor=?, vendor_version=NULL WHERE id=?", id.Vendor, principal)
	return err == nil, err
}
