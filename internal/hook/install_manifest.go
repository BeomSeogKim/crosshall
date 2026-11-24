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
	// Sessions started under an earlier Claude Code revision keep its hooks:
	// claude-1 and claude-2 both offered pending work on UserPromptSubmit.
	if adapter == "claude" && (revision == "claude-1" || revision == "claude-2") {
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

// HasSessionEnd reports whether this revision installs a hook that reports
// the end of a session.
func HasSessionEnd(adapter, revision string) bool {
	// claude-2 installed SessionEnd too.
	if adapter == "claude" && revision == "claude-2" {
		return true
	}
	m, ok := loadManifest(adapter)
	if !ok || m.Revision != revision {
		return false
	}
	for _, name := range m.Install.Events {
		if m.Events[name].Fact == wire.HookSessionEnded {
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
