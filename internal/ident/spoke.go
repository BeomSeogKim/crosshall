package ident

import (
	"context"
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// SpokeLeaf is a session registered on a linked device, as its spoke
// store holds it (#400).
type SpokeLeaf struct {
	Vendor, Key string
	Generation  string
	Epoch       int64
	Membership  core.Membership
}

// RegisterSpokeHello processes first contact on a linked device, in its
// spoke store's transaction. It stages and identifies the leaf directory
// as RegisterHello does, with no principal: the hall's membership lives
// on the home. A session the device registered before may register again
// (a lost tmp leaf, a new vendor version); an unknown one only for a join,
// which the member then sends to the home. A new directory is a new
// registration: a new generation and the next epoch. The caller calls
// Publish after commit.
func RegisterSpokeHello(ctx context.Context, tx store.Tx, rendezvous string, hello wire.Hello, now time.Time) (Registration, error) {
	var reg Registration
	id, err := helloIdentity(hello)
	if err != nil {
		return reg, err
	}
	reg.rendezvous, reg.digest, reg.version = rendezvous, id.Digest(), id.Version
	var storedDigest, storedID string
	var epoch int64
	err = tx.QueryRowContext(ctx, `SELECT digest, file_identity, generation, epoch, binding_id, membership_gen
		FROM spoke_leaf WHERE substr(digest, 1, 16) = ?`, reg.digest[:16]).
		Scan(&storedDigest, &storedID, &reg.generation, &epoch, &reg.membership.BindingID, &reg.membership.Generation)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return reg, err
	}
	if storedDigest != "" && storedDigest != reg.digest {
		return reg, &EnvironmentError{core.CodeLeafMismatch, "member directory registration conflicts"}
	}
	if storedDigest == "" && hello.Intent != "join" {
		return reg, &EnvironmentError{core.CodeNotEnrolled, "this session has not joined the hall"}
	}
	members, err := openMembers(rendezvous)
	if err != nil {
		return reg, err
	}
	defer members.Close()
	leaf, err := OpenPrivateChild(members, reg.digest[:16])
	staging := errors.Is(err, fs.ErrNotExist)
	if staging {
		leaf, _, err = ensureChild(members, reg.stageName())
	}
	if err != nil {
		return reg, err
	}
	defer leaf.Close()
	if reg.fileID, err = FileIdentity(leaf); err != nil {
		return reg, err
	}
	if !staging && storedID != reg.fileID {
		return reg, &EnvironmentError{core.CodeLeafMismatch, "registered member directory was replaced"}
	}
	for _, name := range []string{"in", "work", "out"} {
		child, _, err := ensureChild(leaf, name)
		if err != nil {
			return reg, err
		}
		child.Close()
	}
	if storedID != reg.fileID {
		reg.generation = wire.NewID()
		_, err = tx.ExecContext(ctx, `INSERT INTO spoke_leaf
			(digest, vendor, key, generation, epoch, file_identity, vendor_version, created_at_ms)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(digest) DO UPDATE SET generation = excluded.generation, epoch = excluded.epoch,
				file_identity = excluded.file_identity, created_at_ms = excluded.created_at_ms,
				stamp_binding = 0, stamp_gen = 0, stamp_seq = 0, hook_sent_seq = 0`,
			reg.digest, id.Vendor, id.Key, reg.generation, epoch+1, reg.fileID, reg.version, now.UnixMilli())
	}
	if err == nil && reg.version != "" {
		_, err = tx.ExecContext(ctx, `UPDATE spoke_leaf SET vendor_version = ? WHERE digest = ?`, reg.version, reg.digest)
	}
	return reg, err
}

// ResolveSpokeMember authenticates a member request that arrived on a
// linked device's member spool, as SQLResolver does for the hall: the
// spool's path and directory identity, then the registered generation.
func ResolveSpokeMember(ctx context.Context, tx store.Tx, rendezvous string, ingress Ingress, proof Proof) (SpokeLeaf, error) {
	var leaf SpokeLeaf
	if len(proof.KeyDigest) != 64 || strings.Trim(proof.KeyDigest, "0123456789abcdef") != "" || proof.Generation == "" ||
		ingress.Dir == nil || ingress.Path != filepath.Join(rendezvous, "m", proof.KeyDigest[:16]) {
		return leaf, leafMismatch()
	}
	fi, err := ingress.Dir.Stat(".")
	if err != nil || checkPrivate(fi, os.ModeDir|0700) != nil {
		return leaf, leafMismatch()
	}
	fileID, err := FileIdentity(ingress.Dir)
	if err != nil {
		return leaf, leafMismatch()
	}
	var registered string
	err = tx.QueryRowContext(ctx, `SELECT vendor, key, generation, epoch, file_identity, binding_id, membership_gen
		FROM spoke_leaf WHERE digest = ?`, proof.KeyDigest).
		Scan(&leaf.Vendor, &leaf.Key, &leaf.Generation, &leaf.Epoch, &registered, &leaf.Membership.BindingID, &leaf.Membership.Generation)
	if errors.Is(err, sql.ErrNoRows) {
		return leaf, leafMismatch()
	}
	if err != nil {
		return leaf, err
	}
	if registered != fileID || leaf.Generation != proof.Generation {
		return leaf, leafMismatch()
	}
	return leaf, nil
}

// OpenSpokeLeaf opens the leaf directory of a session registered on a
// linked device, by its registration digest.
func OpenSpokeLeaf(rendezvous, digest string) (*os.Root, error) {
	if len(digest) < 16 {
		return nil, errors.New("ident: short leaf digest")
	}
	members, err := openMembers(rendezvous)
	if err != nil {
		return nil, err
	}
	defer members.Close()
	return OpenPrivateChild(members, digest[:16])
}
