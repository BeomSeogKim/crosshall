package ident

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// Registration describes the directory to make visible after the caller commits.
type Registration struct {
	rendezvous string
	digest     string
	fileID     string
	generation string
	version    string
	membership core.Membership
}

// RegisterHello processes first contact in the dispatcher's transaction. The
// dispatcher calls Publish after commit and before sending its hello response.
func RegisterHello(ctx context.Context, tx store.Tx, rendezvous string, hello wire.Hello, now time.Time) (Registration, error) {
	var reg Registration
	id, err := helloIdentity(hello)
	if err != nil {
		return reg, err
	}
	var principal sql.NullInt64
	var keyState string
	err = tx.QueryRowContext(ctx, "SELECT principal_id, state FROM principal_key WHERE vendor=? AND key=?",
		id.Vendor, id.Key).Scan(&principal, &keyState)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return reg, err
	}
	// A conversation a /clear retired may register its directory for a join;
	// the join itself releases the key, in its own transaction. A start
	// registers an unknown key as a join does, but refuses a retired one as
	// a lookup does: a resumed conversation that a /clear retired does not
	// join again at its start.
	clearedJoin := false
	if keyState == "retired" {
		cleared, err := isCleared(ctx, tx, id)
		if err != nil {
			return reg, err
		}
		if !cleared || hello.Intent != "join" {
			return reg, retiredKey(ctx, tx, id, "session key has been retired")
		}
		clearedJoin = true
	}
	if !principal.Valid && hello.Intent != "join" && hello.Intent != wire.HelloStart {
		return reg, &EnvironmentError{core.CodeNotEnrolled, "this session is not in a channel"}
	}

	reg.rendezvous, reg.digest, reg.version = rendezvous, id.Digest(), id.Version
	var storedDigest, storedID, state string
	var previousPrincipal sql.NullInt64
	// Disk names use a prefix, but the registration always compares the full digest.
	err = tx.QueryRowContext(ctx, "SELECT digest, file_identity, generation, state, principal_id FROM leaf WHERE substr(digest,1,16)=?",
		reg.digest[:16]).Scan(&storedDigest, &storedID, &reg.generation, &state, &previousPrincipal)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return reg, err
	}
	if (storedDigest != "" && storedDigest != reg.digest) || (state == "fenced" && !clearedJoin) {
		return reg, &EnvironmentError{core.CodeLeafMismatch, "member directory registration conflicts"}
	}
	members, err := openMembers(rendezvous)
	if err != nil {
		return reg, err
	}
	defer members.Close()
	leaf, err := OpenPrivateChild(members, reg.digest[:16])
	staging := errors.Is(err, fs.ErrNotExist)
	if staging {
		// The public path remains absent across a failed transaction. A later
		// hello reuses the private staging inode and can finish publication.
		leaf, _, err = ensureChild(members, reg.stageName())
	}
	if err != nil {
		return reg, err
	}
	defer leaf.Close()
	reg.fileID, err = FileIdentity(leaf)
	if err != nil {
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
	}
	if storedID != reg.fileID || previousPrincipal != principal {
		_, err = tx.ExecContext(ctx, `INSERT INTO leaf
			(digest, vendor, key, principal_id, generation, state, file_identity, created_at_ms)
			VALUES (?, ?, ?, ?, ?, 'live', ?, ?)
			ON CONFLICT(digest) DO UPDATE SET principal_id=excluded.principal_id, generation=excluded.generation,
				file_identity=excluded.file_identity, created_at_ms=excluded.created_at_ms`,
			reg.digest, id.Vendor, id.Key, principal, reg.generation, reg.fileID, now.UnixMilli())
	}
	if err == nil && principal.Valid && reg.version != "" {
		_, err = tx.ExecContext(ctx, "UPDATE principal SET vendor_version=? WHERE id=? AND vendor_version IS NOT ?",
			reg.version, principal.Int64, reg.version)
	}
	if err == nil && principal.Valid {
		reg.membership, err = SelectedMembership(ctx, tx, principal.Int64)
	}
	return reg, err
}

func helloIdentity(h wire.Hello) (Identity, error) {
	if h.Version != wire.Version || !validRoute(h.Attempt) || (h.Intent != "join" && h.Intent != "lookup" && h.Intent != wire.HelloStart) {
		return Identity{}, fmt.Errorf("invalid first-contact envelope")
	}
	id, err := parseVendorKey(h.Vendor, h.Key)
	id.Version = h.VendorVersion
	return id, err
}

func parseVendorKey(vendor, key string) (Identity, error) {
	var variable string
	if vendor == "route" {
		variable = RouteVariable
	} else {
		row, _ := indexRow(vendor)
		variable = row.sessionEnv
	}
	if variable == "" {
		return Identity{}, &EnvironmentError{core.CodeNotEnrolled, "unknown identity source"}
	}
	return ResolveEnvironment(func(k string) (string, bool) {
		if k == variable {
			return key, true
		}
		return "", false
	})
}

func openMembers(path string) (*os.Root, error) {
	root, err := OpenPrivateDir(path)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	return OpenPrivateChild(root, "m")
}

func ensureChild(parent *os.Root, name string) (*os.Root, bool, error) {
	err := parent.Mkdir(name, 0700)
	if err != nil && !errors.Is(err, fs.ErrExist) {
		return nil, false, err
	}
	created := err == nil
	child, err := OpenPrivateChild(parent, name)
	return child, created, err
}

func (reg Registration) stageName() string { return ".register-" + reg.digest }

// Publish installs the committed directory and generation before the hello reply.
// Repeating it also completes publication interrupted by a dispatcher restart.
func (reg Registration) Publish() error {
	if !validRoute(reg.generation) || len(reg.digest) != 64 {
		return fmt.Errorf("invalid directory registration")
	}
	members, err := openMembers(reg.rendezvous)
	if err != nil {
		return err
	}
	defer members.Close()
	leafName := reg.digest[:16]
	leaf, err := OpenPrivateChild(members, leafName)
	staging := errors.Is(err, fs.ErrNotExist)
	if staging {
		leaf, err = OpenPrivateChild(members, reg.stageName())
	}
	if err != nil {
		return err
	}
	defer leaf.Close()
	identity, err := FileIdentity(leaf)
	if err != nil {
		return err
	}
	if identity != reg.fileID {
		return &EnvironmentError{core.CodeLeafMismatch, "registered member directory was replaced"}
	}
	if staging {
		if err := members.Rename(reg.stageName(), leafName); err != nil {
			return err
		}
	}
	if reg.version != "" {
		if err := publishMetadata(leaf, "version", reg.version); err != nil {
			return err
		}
	}
	if err := publishGeneration(leaf, reg.generation, true); err != nil {
		return err
	}
	return RepublishMembership(leaf, reg.membership)
}

func publishMetadata(leaf *os.Root, name, value string) error {
	current, err := ReadPrivateFile(leaf, name, 4096)
	if err == nil && string(current) == value {
		return nil
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	stage := wire.NewID() + ".part"
	f, err := leaf.OpenFile(stage, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer leaf.Remove(stage)
	_, err = f.WriteString(value)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return leaf.Rename(stage, name)
}
