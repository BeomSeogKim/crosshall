//go:build e2e

package e2e

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/ident"
)

func TestResetStateRejoinsSameIdentity(t *testing.T) {
	for _, vendor := range []string{"route", "claude"} {
		t.Run(vendor, func(t *testing.T) {
			w := newWorld(t)
			owner := w.owner()
			member := w.member()
			id := ident.Identity{Vendor: vendor, Key: member.route}
			if vendor == "claude" {
				member.route = ""
				member.extra = []string{"CLAUDE_CODE_SESSION_ID=" + id.Key}
			}
			owner.must("owner", "start")
			owner.must("owner", "channel", "create", "checkout")
			member.must("join", "checkout", "--as", "form")

			members := filepath.Join(w.rendezvous(), "m")
			leaf := filepath.Join(members, id.Digest()[:16])
			oldGen, err := os.ReadFile(filepath.Join(leaf, "gen"))
			if err != nil {
				t.Fatal(err)
			}
			stage := filepath.Join(members, ".register-"+(ident.Identity{Vendor: "route", Key: randomHex(16)}).Digest())
			if err := os.Mkdir(stage, 0700); err != nil {
				t.Fatal(err)
			}

			w.kill()
			if err := os.RemoveAll(w.state); err != nil {
				t.Fatal(err)
			}
			owner.must("owner", "start")
			owner.must("owner", "channel", "create", "checkout")
			member.must("join", "checkout", "--as", "form")
			got := member.must("who").aliases()
			if !reflect.DeepEqual(got, []string{"form"}) {
				t.Fatalf("roster after reset: %v", got)
			}
			newGen, err := os.ReadFile(filepath.Join(leaf, "gen"))
			if err != nil || string(newGen) == string(oldGen) {
				t.Fatalf("join did not publish a fresh generation: %q, %v", newGen, err)
			}
			if fi, err := os.Lstat(stage); err != nil || !fi.IsDir() {
				t.Fatalf("startup removed an unrelated registration stage: %v", err)
			}
		})
	}
}
