package hook

import (
	"slices"
	"strings"

	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
)

// InstallSpec describes a vendor's user-level hook file.
type InstallSpec struct {
	Adapter  string
	Revision string
	HomeEnv  string            `json:"home_env"`
	File     string            `json:"file"`
	Encoding string            `json:"encoding"`
	Timeout  int               `json:"timeout_s"`
	Events   []string          `json:"events"`
	Matchers map[string]string `json:"matchers"`
}

// InstallSpecs returns the installation descriptions in adapter order.
func InstallSpecs() []InstallSpec {
	files, _ := manifests.FS.ReadDir(".")
	var specs []InstallSpec
	for _, file := range files {
		m, ok := loadManifest(strings.TrimSuffix(file.Name(), ".json"))
		if !ok || m.Install.File == "" || len(m.Install.Events) == 0 {
			continue
		}
		m.Install.Adapter, m.Install.Revision = m.Adapter, m.Revision
		specs = append(specs, m.Install)
	}
	if agy := slices.IndexFunc(specs, func(spec InstallSpec) bool { return spec.Adapter == "agy" }); agy >= 0 {
		spec := specs[agy]
		specs = append(slices.Delete(specs, agy, agy+1), spec)
	}
	return specs
}
