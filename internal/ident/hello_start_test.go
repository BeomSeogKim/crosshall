package ident

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// A start registers an unknown key, as a join does, and refuses a retired
// key, cleared or not, as a lookup does.
func TestStartJoinsAnUnknownKeyAndRefusesARetiredOne(t *testing.T) {
	for _, tc := range []struct {
		name, setup string
		intent      string
		code        core.Code // "" registers
	}{
		{"unknown, start", "", "start", ""},
		{"unknown, lookup", "", "lookup", core.CodeNotEnrolled},
		{"retired by a clear, start", "retired_by = 'clear'", "start", core.CodeKeyRetired},
		{"retired by a clear, join", "retired_by = 'clear'", "join", ""},
		{"retired, start", "retired_by = NULL", "start", core.CodeKeyRetired},
		{"a member's key, start", "live", "start", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := sqliteFixture(t)
			root := privateTempDir(t)
			if err := os.Mkdir(filepath.Join(root, "m"), 0700); err != nil {
				t.Fatal(err)
			}
			if tc.setup != "" {
				if _, err := db.Exec("INSERT INTO principal(id,kind,vendor,created_at_ms) VALUES(1,'session','codex',0)"); err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec("INSERT INTO principal_key(vendor,key,principal_id,created_at_ms) VALUES('codex','session',1,0)"); err != nil {
					t.Fatal(err)
				}
				if tc.setup != "live" {
					if _, err := db.Exec("UPDATE principal_key SET state = 'retired', retired_at_ms = 1, " + tc.setup); err != nil {
						t.Fatal(err)
					}
				}
			}
			tx, err := db.Begin()
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			h := wire.Hello{Version: wire.Version, Attempt: wire.NewID(), Vendor: "codex", Key: "session", Intent: tc.intent}
			_, err = RegisterHello(t.Context(), tx, root, h, time.Now())
			var env *EnvironmentError
			switch {
			case tc.code == "" && err != nil:
				t.Fatalf("refused: %v", err)
			case tc.code != "" && (!errors.As(err, &env) || env.Code != tc.code):
				t.Fatalf("got %v, want %s", err, tc.code)
			}
		})
	}
}
