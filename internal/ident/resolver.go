package ident

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// SQLResolver authenticates the directory on which the dispatcher received a request.
type SQLResolver struct {
	StateRoot      string
	RendezvousRoot string
	Clock          core.Clock
}

var _ Resolver = SQLResolver{}

func leafMismatch() error {
	return &EnvironmentError{core.CodeLeafMismatch, "member route does not match the registered directory"}
}

func (r SQLResolver) Resolve(ctx context.Context, tx store.Tx, ingress Ingress, proof Proof) (Resolution, error) {
	if ingress.Owner {
		return r.resolveOwner(ctx, tx, ingress, proof)
	}

	if len(proof.KeyDigest) != 64 || strings.Trim(proof.KeyDigest, "0123456789abcdef") != "" || proof.Generation == "" {
		return Resolution{}, leafMismatch()
	}
	root := r.RendezvousRoot
	if root == "" {
		if !filepath.IsAbs(r.StateRoot) {
			return Resolution{}, leafMismatch()
		}
		root = Rendezvous(r.StateRoot, os.Geteuid())
	}
	leafName := proof.KeyDigest[:16]
	if !filepath.IsAbs(root) || ingress.Path != filepath.Join(root, "m", leafName) {
		return Resolution{}, leafMismatch()
	}

	dir := ingress.Dir
	if dir == nil {
		memberRoot, err := OpenPrivateDir(root)
		if err != nil {
			return Resolution{}, leafMismatch()
		}
		defer memberRoot.Close()
		members, err := OpenPrivateChild(memberRoot, "m")
		if err != nil {
			return Resolution{}, leafMismatch()
		}
		defer members.Close()
		dir, err = OpenPrivateChild(members, leafName)
		if err != nil {
			return Resolution{}, leafMismatch()
		}
		defer dir.Close()
	}
	fi, err := dir.Stat(".")
	if err != nil || checkPrivate(fi, os.ModeDir|0700) != nil {
		return Resolution{}, leafMismatch()
	}
	fileID, err := FileIdentity(dir)
	if err != nil {
		return Resolution{}, leafMismatch()
	}

	var id Identity
	var generation, state, registeredFile string
	var bound, principal sql.NullInt64
	var keyState, kind sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT l.vendor, l.key, l.generation, l.state, l.file_identity,
		l.principal_id, k.state, k.principal_id, p.kind
		FROM leaf l LEFT JOIN principal_key k ON k.vendor=l.vendor AND k.key=l.key
		LEFT JOIN principal p ON p.id=k.principal_id WHERE l.digest=?`, proof.KeyDigest).
		Scan(&id.Vendor, &id.Key, &generation, &state, &registeredFile, &bound, &keyState, &principal, &kind)
	if errors.Is(err, sql.ErrNoRows) {
		return Resolution{}, leafMismatch()
	}
	if err != nil {
		return Resolution{}, err
	}
	if registeredFile != fileID || id.Digest() != proof.KeyDigest {
		return Resolution{}, leafMismatch()
	}
	if keyState.String == "retired" {
		if proof.Join && state == "fenced" && generation == proof.Generation && bound.Valid {
			if res, ok, err := r.clearedJoin(ctx, tx, root, id, proof, fileID, bound.Int64); ok || err != nil {
				return res, err
			}
		}
		return Resolution{}, retiredKey(ctx, tx, id, "session identity is retired")
	}
	if state != "live" || generation != proof.Generation || (bound.Valid && bound != principal) {
		return Resolution{}, leafMismatch()
	}
	if !principal.Valid {
		return Resolution{
			Caller: core.Caller{Role: core.RoleUnenrolled},
			Vendor: id.Vendor,
			Key:    id.Key,
			Enroll: r.enrollment(tx, id, proof, fileID),
		}, nil
	}
	if keyState.String != "live" || kind.String != "session" {
		return Resolution{}, leafMismatch()
	}

	caller := core.Caller{Role: core.RoleUnenrolled, PrincipalID: &principal.Int64}
	var selected core.Membership
	var live bool
	err = tx.QueryRowContext(ctx, `SELECT s.binding_id, s.generation, b.state = 'live'
		FROM member_selection s JOIN binding b ON b.id = s.binding_id AND b.principal_id = s.principal_id
		WHERE s.principal_id = ?`, principal.Int64).Scan(&selected.BindingID, &selected.Generation, &live)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return Resolution{}, err
	}
	if err == nil {
		caller.Membership = &selected
	}
	if live {
		caller.Role = core.RoleMember
		caller.BindingID = &selected.BindingID
	}
	return Resolution{Caller: caller, Vendor: id.Vendor, Key: id.Key}, nil
}

func (r SQLResolver) resolveOwner(ctx context.Context, tx store.Tx, ingress Ingress, proof Proof) (Resolution, error) {
	refused := &EnvironmentError{core.CodeOwnerOnly, "request did not arrive through the configured owner directory"}
	if !filepath.IsAbs(r.StateRoot) || ingress.Path != filepath.Join(r.StateRoot, "owner") || proof.KeyDigest != "" || proof.Generation != "" {
		return Resolution{}, refused
	}
	root, err := OpenPrivateDir(r.StateRoot)
	if err != nil {
		return Resolution{}, refused
	}
	defer root.Close()
	owner, err := OpenPrivateChild(root, "owner")
	if err != nil {
		return Resolution{}, refused
	}
	defer owner.Close()
	if ingress.Dir != nil {
		expected, err := FileIdentity(owner)
		if err != nil {
			return Resolution{}, refused
		}
		actual, err := FileIdentity(ingress.Dir)
		if err != nil || actual != expected {
			return Resolution{}, refused
		}
	}
	caller := core.Caller{Role: core.RoleOwner}
	var principal int64
	err = tx.QueryRowContext(ctx, "SELECT id FROM principal WHERE kind='owner'").Scan(&principal)
	if err == nil {
		caller.PrincipalID = &principal
	} else if !errors.Is(err, sql.ErrNoRows) {
		return Resolution{}, err
	}
	return Resolution{Caller: caller, Vendor: "owner"}, nil
}

func (r SQLResolver) enrollment(tx store.Tx, id Identity, proof Proof, fileID string) func(context.Context, store.Tx, int64) error {
	return func(ctx context.Context, target store.Tx, principal int64) error {
		if target != tx {
			return errors.New("enrollment belongs to another transaction")
		}
		clock := r.Clock
		if clock == nil {
			clock = time.Now
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO principal_key (vendor,key,principal_id,state,created_at_ms)
			SELECT ?,?,id,'live',? FROM principal WHERE id=? AND kind='session' AND vendor=?`,
			id.Vendor, id.Key, clock().UnixMilli(), principal, id.Vendor)
		if err := exactlyOne(result, err); err != nil {
			return err
		}
		result, err = tx.ExecContext(ctx, `UPDATE leaf SET principal_id=?
			WHERE digest=? AND generation=? AND file_identity=? AND state='live' AND principal_id IS NULL`,
			principal, proof.KeyDigest, proof.Generation, fileID)
		if err := exactlyOne(result, err); err != nil {
			return err
		}
		if proof.VendorVersion != "" {
			_, err = tx.ExecContext(ctx, "UPDATE principal SET vendor_version=? WHERE id=?", proof.VendorVersion, principal)
		}
		return err
	}
}

func exactlyOne(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return fmt.Errorf("expected one enrollment row, changed %d", n)
	}
	return err
}
