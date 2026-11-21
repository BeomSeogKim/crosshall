package store

import (
	"os"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func TestInstallConsentReadsLegacyOwnerChoicesWithoutMigrating(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	db, err := openWithMigrations(ctx, root, migrations[:12])
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	seedAdapterLedger(t, db)
	err = db.Update(ctx, func(tx Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO op (principal_id, op_key, scope, verb, payload_hash, result, at_ms)
			VALUES (1, 'explicit-off', 'channel:auto', 'owner channel auto', 'fixture', '{}', 1);
			INSERT INTO capability_evidence (vendor, vendor_version, platform, mode, manifest_rev, cap, level, result_ref, checked_at_ms)
			VALUES ('fixture', '2', 'darwin-arm64', 'print', 'fixture-1', 'turn_end', 'unavailable', 'owner', 2);`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	profile := wire.HookProfile{Adapter: "fixture", VendorVersion: "2", Platform: "darwin-arm64", ManifestRev: "fixture-1"}
	consent, err := InspectInstallConsent(ctx, os.DirFS(root), profile,
		[]BuiltinEvidence{{Vendor: "fixture", Version: "2", Platform: "darwin-arm64", Mode: "print", ManifestRev: "fixture-1", Capability: "turn_end"}})
	if err != nil || !consent.AutoJoinOff || consent.TurnEndCovered {
		t.Fatalf("consent %+v, %v", consent, err)
	}
	var version int
	if err := db.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != 12 {
		t.Fatalf("schema version %d, %v; want 12", version, err)
	}
}
