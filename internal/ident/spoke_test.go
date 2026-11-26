package ident

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// A linked device registers its sessions without a principal: a lookup of
// an unknown session is refused, a join registers it, a lost leaf
// registers again with a new generation and the next epoch, and the
// registered directory authenticates member requests.
func TestASpokeRegistersItsOwnSessions(t *testing.T) {
	db, err := store.OpenSpoke(t.Context(), privateTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	root := privateTempDir(t)
	if err := os.Mkdir(filepath.Join(root, "m"), 0700); err != nil {
		t.Fatal(err)
	}
	hello := func(intent string) (Registration, error) {
		t.Helper()
		var reg Registration
		err := db.Update(t.Context(), func(tx store.Tx) error {
			var err error
			reg, err = RegisterSpokeHello(t.Context(), tx, root,
				wire.Hello{Version: wire.Version, Attempt: wire.NewID(), Vendor: "codex", Key: "session", Intent: intent}, time.Now())
			return err
		})
		if err == nil {
			err = reg.Publish()
		}
		return reg, err
	}
	var env *EnvironmentError
	if _, err := hello("lookup"); !errors.As(err, &env) || env.Code != core.CodeNotEnrolled {
		t.Fatalf("lookup of an unknown session: %v", err)
	}
	first, err := hello("join")
	if err != nil {
		t.Fatal(err)
	}
	if again, err := hello("lookup"); err != nil || again.generation != first.generation {
		t.Fatalf("a lookup of a registered session changed it: %v", err)
	}
	resolve := func(reg Registration) (SpokeLeaf, error) {
		t.Helper()
		path := filepath.Join(root, "m", reg.digest[:16])
		dir, err := os.OpenRoot(path)
		if err != nil {
			t.Fatal(err)
		}
		defer dir.Close()
		var leaf SpokeLeaf
		err = db.Update(t.Context(), func(tx store.Tx) error {
			var err error
			leaf, err = ResolveSpokeMember(t.Context(), tx, root, Ingress{Path: path, Dir: dir},
				Proof{KeyDigest: reg.digest, Generation: reg.generation})
			return err
		})
		return leaf, err
	}
	if leaf, err := resolve(first); err != nil || leaf.Epoch != 1 || leaf.Key != "session" {
		t.Fatalf("resolve %+v, %v", leaf, err)
	}
	// The tmp leaf is lost: the session registers again, and what the old
	// leaf's publish counter marked (its stamp boundary, its released hook
	// sequence) does not carry over to the new counter.
	marks := func(set bool) (n int) {
		t.Helper()
		if err := db.Update(t.Context(), func(tx store.Tx) error {
			if set {
				_, err := tx.ExecContext(t.Context(), `UPDATE spoke_leaf SET stamp_binding = 1, stamp_gen = 1, stamp_seq = 9, hook_sent_seq = 9`)
				return err
			}
			return tx.QueryRowContext(t.Context(), `SELECT stamp_binding + stamp_gen + stamp_seq + hook_sent_seq FROM spoke_leaf`).Scan(&n)
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	marks(true)
	if err := os.Rename(filepath.Join(root, "m", first.digest[:16]), filepath.Join(root, "lost")); err != nil {
		t.Fatal(err)
	}
	second, err := hello("lookup")
	if err != nil || second.generation == first.generation {
		t.Fatalf("re-registration %v", err)
	}
	if leaf, err := resolve(second); err != nil || leaf.Epoch != 2 {
		t.Fatalf("resolve after re-registration %+v, %v", leaf, err)
	}
	if n := marks(false); n != 0 {
		t.Fatalf("the new generation kept the old leaf's marks (%d)", n)
	}
	if _, err := resolve(first); err == nil {
		t.Fatal("the old generation still authenticates")
	}
}
