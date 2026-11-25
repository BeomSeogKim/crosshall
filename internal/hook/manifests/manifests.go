// Package manifests holds the hook manifest of each adapter this build
// supports. An adapter is supported exactly when it has a manifest here.
package manifests

import (
	"embed"
	"io/fs"
	"slices"
	"strings"
)

// FS holds the manifests this build ships.
//
//go:embed *.json
var FS embed.FS

// bundles holds the manifests that adapter bundles built in under a build
// tag add, each from a file of this package that only that tag compiles.
// A shipped manifest takes precedence over a bundle's of the same name.
var bundles []fs.FS

// Names lists the supported adapters, sorted.
func Names() []string {
	names := namesIn(FS)
	for _, source := range bundles {
		for _, name := range namesIn(source) {
			if !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	if len(bundles) > 0 {
		slices.Sort(names)
	}
	return names
}

func namesIn(source fs.FS) []string {
	files, _ := fs.ReadDir(source, ".")
	var names []string
	for _, file := range files {
		if name, ok := strings.CutSuffix(file.Name(), ".json"); ok {
			names = append(names, name)
		}
	}
	return names
}

// readSource reads adapter's manifest from the first source that has it.
func readSource(adapter string) ([]byte, error) {
	raw, err := FS.ReadFile(adapter + ".json")
	for _, source := range bundles {
		if err == nil {
			break
		}
		raw, err = fs.ReadFile(source, adapter+".json")
	}
	return raw, err
}

// Has reports whether adapter has a manifest.
func Has(adapter string) bool { return slices.Contains(Names(), adapter) }
