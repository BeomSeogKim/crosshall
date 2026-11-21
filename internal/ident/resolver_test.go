package ident

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

func requireRefusal(t *testing.T, err error, code core.Code) {
	t.Helper()
	var refusal *EnvironmentError
	if !errors.As(err, &refusal) || refusal.Code != code {
		t.Fatalf("got %v, want %s", err, code)
	}
}

func privateTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestResolverChecksReceivingDirectory(t *testing.T) {
	root := privateTempDir(t)
	owner := filepath.Join(root, "owner")
	if err := os.Mkdir(owner, 0700); err != nil {
		t.Fatal(err)
	}
	other, err := os.OpenRoot(privateTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	r := SQLResolver{StateRoot: root, RendezvousRoot: root}
	proof := Proof{Generation: "generation", KeyDigest: Identity{Vendor: "route", Key: "member"}.Digest()}
	for _, tc := range []struct {
		ingress Ingress
		proof   Proof
		code    core.Code
	}{
		{Ingress{Path: root, Owner: true}, Proof{}, core.CodeOwnerOnly},
		{Ingress{Path: owner, Owner: true}, proof, core.CodeOwnerOnly},
		{Ingress{Path: owner, Owner: true, Dir: other}, Proof{}, core.CodeOwnerOnly},
		{Ingress{Path: owner}, proof, core.CodeLeafMismatch},
		{Ingress{Path: root}, Proof{}, core.CodeLeafMismatch},
	} {
		// Invalid paths/proofs must fail before reading from the transaction.
		_, err := r.Resolve(t.Context(), nil, tc.ingress, tc.proof)
		requireRefusal(t, err, tc.code)
	}
}

func sqliteFixture(t *testing.T) *sql.DB {
	t.Helper()
	if !slices.Contains(sql.Drivers(), "sqlite") {
		t.Skip("requires the SQLite store implementation")
	}
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	db.SetMaxOpenConns(1)
	// Identity reads tables of several migrations: apply them all, in order.
	names, err := filepath.Glob("../store/migrations/*.sql")
	if err != nil || len(names) == 0 {
		t.Fatalf("migrations: %v", err)
	}
	slices.Sort(names)
	for _, name := range names {
		schema, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec("PRAGMA foreign_keys=ON;" + string(schema)); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestResolverEnrollmentTransaction(t *testing.T) {
	db := sqliteFixture(t)
	r := SQLResolver{StateRoot: privateTempDir(t), RendezvousRoot: privateTempDir(t)}
	id := Identity{Vendor: "codex", Key: "session"}
	proof := Proof{KeyDigest: id.Digest(), Generation: "first", VendorVersion: "0.170.0"}
	path := filepath.Join(r.RendezvousRoot, "m", proof.KeyDigest[:16])
	if err := os.MkdirAll(path, 0700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.OpenRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	defer dir.Close()
	fileID, err := FileIdentity(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO leaf(digest,vendor,key,generation,file_identity,created_at_ms) VALUES (?,?,?,?,?,0)`,
		proof.KeyDigest, id.Vendor, id.Key, proof.Generation, fileID)
	if err != nil {
		t.Fatal(err)
	}
	begin := func() *sql.Tx {
		t.Helper()
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { tx.Rollback() })
		return tx
	}
	ctx := context.Background()
	ingress := Ingress{Path: path, Dir: dir}
	for _, commit := range []bool{false, true} {
		tx := begin()
		first, err := r.Resolve(ctx, tx, ingress, proof)
		if err != nil || first.Caller.Role != core.RoleUnenrolled || first.Vendor != id.Vendor || first.Enroll == nil {
			t.Fatalf("provisional identity: %+v, %v", first.Caller, err)
		}
		if _, err = tx.Exec("INSERT INTO principal(id,kind,vendor,created_at_ms) VALUES (1,'session','codex',0)"); err != nil {
			t.Fatal(err)
		}
		if err = first.Enroll(ctx, tx, 1); err != nil {
			t.Fatal(err)
		}
		if commit {
			err = tx.Commit()
		} else {
			err = tx.Rollback()
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	tx := begin()
	var version string
	if err := tx.QueryRow("SELECT vendor_version FROM principal WHERE id=1").Scan(&version); err != nil || version != proof.VendorVersion {
		t.Fatalf("missing enrollment version %q: %v", version, err)
	}
	if _, err := tx.Exec(`INSERT INTO channel(id,name,created_at_ms) VALUES(2,'work',0);
		INSERT INTO binding(id,principal_id,channel_id,alias,joined_at_ms) VALUES(3,1,2,'worker',0)`); err != nil {
		t.Fatal(err)
	}
	member, err := r.Resolve(ctx, tx, Ingress{Path: path}, proof)
	if err != nil || member.Caller.Role != core.RoleMember || member.Caller.PrincipalID == nil ||
		*member.Caller.PrincipalID != 1 || member.Caller.BindingID == nil || *member.Caller.BindingID != 3 || member.Enroll != nil {
		t.Fatalf("bound identity: %+v, %v", member.Caller, err)
	}
	wrong, err := os.OpenRoot(privateTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	defer wrong.Close()
	_, err = r.Resolve(ctx, tx, Ingress{Path: path, Dir: wrong}, proof)
	requireRefusal(t, err, core.CodeLeafMismatch)
	old := proof
	old.Generation = "old"
	_, err = r.Resolve(ctx, tx, ingress, old)
	requireRefusal(t, err, core.CodeLeafMismatch)
	ok, err := RestoreKey(ctx, tx, 1, "claude", "next-session", time.Now())
	if err != nil || !ok {
		t.Fatalf("restore did not attach a new key: %v", err)
	}
	var state string
	if err := tx.QueryRow("SELECT state FROM leaf WHERE digest=?", id.Digest()).Scan(&state); err != nil || state != "fenced" {
		t.Fatalf("old leaf state %q after restore: %v", state, err)
	}
	_, err = r.Resolve(ctx, tx, ingress, proof)
	requireRefusal(t, err, core.CodeKeyRetired)
}
