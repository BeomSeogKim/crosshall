package hook

import "github.com/BeomSeogKim/crosshall/internal/hook/manifests"

// HasPromptLine reports whether this revision installs a hook that can
// offer pending work in a user prompt; earlier revisions answer as their
// manifest's prior_revisions declare.
func HasPromptLine(adapter, revision string) bool {
	caps, _ := manifests.CapabilitiesAt(adapter, revision)
	return caps.PromptLine
}

// HasSessionEnd reports whether this revision installs a hook that reports
// the end of a session.
func HasSessionEnd(adapter, revision string) bool {
	caps, _ := manifests.CapabilitiesAt(adapter, revision)
	return caps.SessionEnd
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

// InstallSpecs returns the installation descriptions in setup order.
func InstallSpecs() []InstallSpec {
	var specs []InstallSpec
	for _, adapter := range manifests.SetupOrder() {
		m, ok := loadManifest(adapter)
		if !ok || m.Install.File == "" || len(m.Install.Events) == 0 {
			continue
		}
		m.Install.Adapter, m.Install.Revision = m.Adapter, m.Revision
		specs = append(specs, m.Install)
	}
	return specs
}
