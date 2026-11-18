package ident

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func TestHelloAfterRollbackAndInterruptedPublication(t *testing.T) {
	db := sqliteFixture(t)
	root := privateTempDir(t)
	if err := os.Mkdir(filepath.Join(root, "m"), 0700); err != nil {
		t.Fatal(err)
	}
	h := wire.Hello{Version: wire.Version, Attempt: wire.NewID(), Vendor: "codex", VendorVersion: "0.170.0", Key: "session", Intent: "join"}
	register := func(commit bool) Registration {
		t.Helper()
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback()
		reg, err := RegisterHello(t.Context(), tx, root, h, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		if commit {
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
		}
		return reg
	}

	reg := register(true)
	if err := reg.Publish(); err != nil {
		t.Fatal(err)
	}
	public := filepath.Join(root, "m", reg.digest[:16])
	// Keep the old inode pinned so that this test exercises actual replacement.
	if err := os.Rename(public, filepath.Join(root, "old")); err != nil {
		t.Fatal(err)
	}
	register(false)
	if _, err := os.Lstat(public); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("uncommitted replacement is visible: %v", err)
	}
	pending := register(true)
	again := register(true) // dispatcher restarted before Publish
	if again.generation == reg.generation || again.generation != pending.generation {
		t.Fatal("retry did not recover the committed replacement generation")
	}
	if err := again.Publish(); err != nil {
		t.Fatal(err)
	}
	if err := reg.Publish(); err == nil {
		t.Fatal("old registration accepted the replacement")
	}
	if _, err := db.Exec("INSERT INTO principal(id,kind,vendor,created_at_ms) VALUES(1,'session','codex',0)"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO principal_key(vendor,key,principal_id,created_at_ms) VALUES('codex',?,1,0)", h.Key); err != nil {
		t.Fatal(err)
	}
	h.VendorVersion = "0.172.0"
	newVersion := register(true)
	if err := newVersion.Publish(); err != nil {
		t.Fatal(err)
	}
	var recorded string
	if err := db.QueryRow("SELECT vendor_version FROM principal WHERE id=1").Scan(&recorded); err != nil || recorded != h.VendorVersion {
		t.Fatalf("owner diagnostics have version %q, error %v", recorded, err)
	}
	v, err := os.ReadFile(filepath.Join(public, "version"))
	if err != nil || string(v) != h.VendorVersion || newVersion.generation != again.generation {
		t.Fatalf("version refresh changed membership: version %q, err %v", v, err)
	}
}
