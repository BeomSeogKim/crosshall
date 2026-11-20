// Package manifests holds the hook manifest of each adapter this build
// supports. An adapter is supported exactly when it has a manifest here.
package manifests

import (
	"embed"
	"slices"
	"strings"
)

//go:embed *.json
var FS embed.FS

// Names lists the supported adapters, sorted.
func Names() []string {
	files, _ := FS.ReadDir(".")
	var names []string
	for _, file := range files {
		if name, ok := strings.CutSuffix(file.Name(), ".json"); ok {
			names = append(names, name)
		}
	}
	return names
}

// Has reports whether adapter has a manifest.
func Has(adapter string) bool { return slices.Contains(Names(), adapter) }
