package store

import "testing"

func TestLinkOfferTriggerMigrationChecksExistingEnrollment(t *testing.T) {
	for _, tc := range []struct {
		name     string
		enrolled bool
	}{{"unlinked", false}, {"enrolled", true}} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, root := t.Context(), t.TempDir()
			before, err := openWithMigrations(ctx, root, migrations[:33])
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = before.Close() })
			if tc.enrolled {
				if _, err := before.db.ExecContext(ctx, `INSERT INTO device (device_id, name, secret, created_at_ms, secret_at_ms)
					VALUES ('existing', 'existing', zeroblob(32), 0, 0)`); err != nil {
					t.Fatal(err)
				}
			}
			if err := before.Close(); err != nil {
				t.Fatal(err)
			}
			after, err := Open(ctx, root)
			if err != nil {
				t.Fatal(err)
			}
			defer after.Close()
			if n := after.count(t, `SELECT COUNT(*) FROM sqlite_schema WHERE type = 'trigger'
				AND name IN ('device_offer_pickup', 'device_offer_pending_insert')`); tc.enrolled && n != 2 || !tc.enrolled && n != 0 {
				t.Fatalf("enrolled %t: found %d hot-path offer triggers", tc.enrolled, n)
			}
		})
	}
}
