//go:build synthadapter

package manifests

import (
	"embed"
	"io/fs"
)

// The synth fixture is a fresh adapter ID that proves an adapter bundle
// needs no core edit. Only the synthadapter tag builds it in.
//
//go:embed synth/synth.json
var synthFS embed.FS

func init() {
	sub, err := fs.Sub(synthFS, "synth")
	if err != nil {
		panic(err)
	}
	bundles = append(bundles, sub)
}
