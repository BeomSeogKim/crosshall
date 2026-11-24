package hook

import (
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
)

// Every native identity proof a manifest names has a handler in this
// build, and every declared dialect field is consistent: a list-shaped
// workspace needs a named field.
func TestManifestDialectsNameKnownHandlers(t *testing.T) {
	for _, adapter := range manifests.Names() {
		m, ok := loadManifest(adapter)
		if !ok {
			t.Fatalf("%s: manifest not readable", adapter)
		}
		if _, known := identityProofs[m.IdentityProof]; m.IdentityProof != "" && !known {
			t.Errorf("%s: unknown identity proof %q", adapter, m.IdentityProof)
		}
		if m.CWDList && m.CWDField == "" {
			t.Errorf("%s: cwd_list without cwd_field", adapter)
		}
	}
}
