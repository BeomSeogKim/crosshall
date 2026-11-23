package hook

import (
	"slices"
	"strings"

	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// HasPromptLine reports whether this revision installs a hook that can
// offer pending work in a user prompt.
func HasPromptLine(adapter, revision string) bool {
	// The v0.1.0 Claude hook already offered pending work on UserPromptSubmit.
	if adapter == "claude" && revision == "claude-1" {
		return true
	}
	m, ok := loadManifest(adapter)
	if !ok || m.Revision != revision {
		return false
	}
	for _, name := range m.Install.Events {
		event := m.Events[name]
		if event.Fact == wire.HookPromptSubmitted && event.Notice &&
			(event.Output == "context" || event.Output == "inject_steps") {
			return true
		}
	}
	return false
}

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

// RoutineMemberVerbs defines which commands setup can grant to an adapter.
func RoutineMemberVerbs() []string {
	return []string{"ask", "inbox", "join", "leave", "log", "note", "read", "reply", "status", "wait", "waiting", "who"}
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

// SessionEndAdapters lists the adapters whose installed hooks report a
// session end: while such an adapter is enabled, its hook ends a verified
// launch.
func SessionEndAdapters() []string {
	var adapters []string
	for _, spec := range InstallSpecs() {
		m, _ := loadManifest(spec.Adapter)
		if slices.ContainsFunc(spec.Events, func(name string) bool { return m.Events[name].Fact == wire.HookSessionEnded }) {
			adapters = append(adapters, spec.Adapter)
		}
	}
	return adapters
}
