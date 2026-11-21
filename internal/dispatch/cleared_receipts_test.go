package dispatch

import (
	"fmt"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// Issue #164: the receipts in a directory of a key that a /clear retired are
// read for ten minutes after the clear, then no longer.
func TestClearedReceiptsAreReadWithinTheWindowOnly(t *testing.T) {
	s, root := adapterFilesFixture(t)
	old := ident.Identity{Vendor: "claude", Key: "before-clear"}
	members, err := spool.OpenServerChild(root, "m", false)
	if err != nil {
		t.Fatal(err)
	}
	defer members.Close()
	leaf, err := spool.OpenServerChild(members, old.Digest()[:16], true)
	if err != nil {
		t.Fatal(err)
	}
	defer leaf.Close()
	for name, value := range map[string]string{"gen": "old-gen\n", "ack": "7\n", "rules": "5\n"} {
		if err := leaf.WriteFile(name, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	id, err := spool.DirectoryIdentity(leaf)
	if err != nil {
		t.Fatal(err)
	}
	cleared := time.UnixMilli(1_800_000_000_000)
	s.fixtureSQL(t, fmt.Sprintf(`
		INSERT INTO principal (id, kind, vendor, created_at_ms) VALUES (1, 'session', 'claude', 1);
		INSERT INTO principal_key (vendor, key, principal_id, state, created_at_ms, retired_at_ms, retired_by)
		VALUES ('claude', 'before-clear', 1, 'retired', 1, %d, 'clear');
		INSERT INTO leaf (digest, vendor, key, principal_id, generation, state, file_identity, created_at_ms)
		VALUES ('%s', 'claude', 'before-clear', 1, 'old-gen', 'fenced', '%s', 1)
	`, cleared.UnixMilli(), old.Digest(), id))
	principal, binding := int64(1), int64(1)
	read := func(now time.Time) (cursor, rules uint64) {
		t.Helper()
		if err := s.db.Update(t.Context(), func(tx store.Tx) (err error) {
			cursor, rules, err = s.clearedReceipts(t.Context(), tx, core.Caller{PrincipalID: &principal, BindingID: &binding}, now)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return cursor, rules
	}
	if cursor, rules := read(cleared.Add(9 * time.Minute)); cursor != 7 || rules != 5 {
		t.Fatalf("within the window read %d/%d, want 7/5", cursor, rules)
	}
	if cursor, rules := read(cleared.Add(11 * time.Minute)); cursor != 0 || rules != 0 {
		t.Fatalf("after the window read %d/%d, want nothing", cursor, rules)
	}
}
