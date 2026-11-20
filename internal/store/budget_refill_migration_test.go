package store

import (
	"testing"
	"time"
)

func TestRootRefillMigrationStartsExistingMembersAtUpgrade(t *testing.T) {
	ctx := t.Context()
	root := t.TempDir()
	prior, err := openWithMigrations(ctx, root, migrations[:8])
	if err != nil {
		t.Fatal(err)
	}
	seedAdapterLedger(t, prior)
	before := adapterLedgerSnapshot(t, prior.db, 8)
	if err := prior.Close(); err != nil {
		t.Fatal(err)
	}
	upgradeStarted := time.Now().UnixMilli()
	upgraded, err := openWithMigrations(ctx, root, migrations[:9])
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	if after := adapterLedgerSnapshot(t, upgraded.db, 9); after != before {
		t.Fatalf("migration changed existing credit or history\nbefore: %s\nafter: %s", before, after)
	}
	var earliest, latest int64
	if err := upgraded.db.QueryRowContext(ctx, `SELECT MIN(root_refill_at_ms), MAX(root_refill_at_ms)
		FROM principal WHERE kind = 'session'`).Scan(&earliest, &latest); err != nil {
		t.Fatal(err)
	}
	if earliest < upgradeStarted-1 || latest > time.Now().UnixMilli() || earliest != latest {
		t.Fatalf("refill anchors %d through %d, upgrade started %d", earliest, latest, upgradeStarted)
	}
	var rate, ceiling int64
	if err := upgraded.db.QueryRowContext(ctx, `SELECT root_refill_per_hour, root_refill_ceiling FROM channel WHERE id = 1`).
		Scan(&rate, &ceiling); err != nil || rate != 3 || ceiling != 5 {
		t.Fatalf("existing channel policy: %d/h up to %d (%v)", rate, ceiling, err)
	}
}
