package manifests

import (
	"cmp"
	"encoding/json"
	"slices"
	"sync"
)

// Schema is the manifest format this build reads.
const Schema = 1

// Capabilities are what an adapter's manifest declares that the core asks
// about. The core reads them here instead of testing adapter names, so an
// adapter gains a behaviour by declaring it.
type Capabilities struct {
	// ProcessLaunch: the manifest names the session's process
	// ("launch_env"), so a launch can be bound to it and its exit tracked.
	ProcessLaunch bool
	// TurnEnd: an installed event checks the end of a turn ("stop").
	TurnEnd bool
	// RecoverEnroll: an installed event may enroll a session that missed
	// its start ("recover_enroll").
	RecoverEnroll bool
	// RecoverEvent names that event; a prior revision that declares
	// recover_enroll recovers through the shipped revision's event.
	RecoverEvent string
	// SharedDaemon: the vendor can run sessions in a shared managed server
	// ("daemon"), whose hooks inherit that server's environment.
	SharedDaemon bool
	// PromptLine: an installed prompt event can offer pending work in the
	// prompt ("notice" with context or inject_steps output).
	PromptLine bool
	// SessionEnd: an installed event reports the end of a session.
	SessionEnd bool
}

// capabilityNames are the names a manifest's prior_revisions may list.
var capabilityNames = map[string]func(*Capabilities){
	"process_launch": func(c *Capabilities) { c.ProcessLaunch = true },
	"turn_end":       func(c *Capabilities) { c.TurnEnd = true },
	"recover_enroll": func(c *Capabilities) { c.RecoverEnroll = true },
	"shared_daemon":  func(c *Capabilities) { c.SharedDaemon = true },
	"prompt_line":    func(c *Capabilities) { c.PromptLine = true },
	"session_end":    func(c *Capabilities) { c.SessionEnd = true },
}

type record struct {
	schema     int
	revision   string
	caps       Capabilities
	prior      map[string]Capabilities
	display    string
	aliases    []string
	setupOrder int
	// invalid names what the manifest declares that this build cannot read.
	invalid []string
}

// CapabilitiesOf returns the capabilities adapter's shipped revision
// declares; ok is false when the build has no readable manifest for it.
func CapabilitiesOf(adapter string) (caps Capabilities, ok bool) {
	r, ok := records()[adapter]
	return r.caps, ok
}

// CapabilitiesAt returns the capabilities of one revision of adapter: the
// shipped one, or a prior one its manifest still declares, as for a stored
// profile. ok is false for a revision the manifest does not know; its
// capabilities are then empty.
func CapabilitiesAt(adapter, revision string) (caps Capabilities, ok bool) {
	r, found := records()[adapter]
	switch {
	case !found:
		return Capabilities{}, false
	case revision == r.revision:
		return r.caps, true
	}
	caps, ok = r.prior[revision]
	return caps, ok
}

// DisplayName is the adapter's name for people, or the adapter ID when its
// manifest gives none.
func DisplayName(adapter string) string {
	if r := records()[adapter]; r.display != "" {
		return r.display
	}
	return adapter
}

// Resolve maps a name a person typed, an adapter ID or one of its aliases,
// to the adapter ID; any other name comes back unchanged.
func Resolve(name string) string {
	for _, adapter := range Names() {
		if slices.Contains(records()[adapter].aliases, name) {
			return adapter
		}
	}
	return name
}

// SetupOrder lists the adapters in the order setup presents them.
func SetupOrder() []string {
	names := Names()
	slices.SortStableFunc(names, func(a, b string) int {
		return cmp.Compare(records()[a].setupOrder, records()[b].setupOrder)
	})
	return names
}

var (
	readMu sync.Mutex
	reader = func(adapter string) ([]byte, error) { return FS.ReadFile(adapter + ".json") }
	loaded func() map[string]record
)

// Read returns adapter's manifest bytes. Every decode of a manifest reads
// through it.
func Read(adapter string) ([]byte, error) {
	readMu.Lock()
	read := reader
	readMu.Unlock()
	return read(adapter)
}

// SwapReader replaces Read for a test and returns the function that puts
// the previous reader back. Both drop the decoded catalog, so the next
// lookup decodes again through the reader in place.
func SwapReader(read func(adapter string) ([]byte, error)) (restore func()) {
	readMu.Lock()
	defer readMu.Unlock()
	previous := reader
	reader, loaded = read, nil
	return func() {
		readMu.Lock()
		defer readMu.Unlock()
		reader, loaded = previous, nil
	}
}

// records is the decoded catalog, decoded once on first use.
func records() map[string]record {
	readMu.Lock()
	if loaded == nil {
		loaded = sync.OnceValue(decodeAll)
	}
	load := loaded
	readMu.Unlock()
	return load()
}

func decodeAll() map[string]record {
	all := make(map[string]record)
	for _, adapter := range Names() {
		raw, err := Read(adapter)
		if err != nil {
			continue
		}
		var m struct {
			Schema         int                 `json:"schema"`
			Adapter        string              `json:"adapter"`
			Revision       string              `json:"revision"`
			DisplayName    string              `json:"display_name"`
			Aliases        []string            `json:"aliases"`
			SetupOrder     int                 `json:"setup_order"`
			PriorRevisions map[string][]string `json:"prior_revisions"`
			LaunchEnv      string              `json:"launch_env"`
			Daemon         json.RawMessage     `json:"daemon"`
			Install        struct {
				Events   []string          `json:"events"`
				Matchers map[string]string `json:"matchers"`
			} `json:"install"`
			Events map[string]struct {
				Fact          string `json:"fact"`
				Output        string `json:"output"`
				Notice        bool   `json:"notice"`
				Stop          bool   `json:"stop"`
				RecoverEnroll bool   `json:"recover_enroll"`
			} `json:"events"`
		}
		if json.Unmarshal(raw, &m) != nil || m.Adapter != adapter || m.Revision == "" {
			continue
		}
		r := record{schema: m.Schema, revision: m.Revision, display: m.DisplayName, aliases: m.Aliases,
			setupOrder: m.SetupOrder, prior: make(map[string]Capabilities)}
		if m.Schema != Schema {
			r.invalid = append(r.invalid, "unsupported schema")
		}
		r.caps.ProcessLaunch = m.LaunchEnv != ""
		r.caps.SharedDaemon = len(m.Daemon) > 0 && string(m.Daemon) != "null"
		for _, name := range m.Install.Events {
			event, known := m.Events[name]
			if !known {
				r.invalid = append(r.invalid, "installed event "+name+" is not declared")
			}
			r.caps.TurnEnd = r.caps.TurnEnd || event.Stop
			if event.RecoverEnroll {
				if r.caps.RecoverEnroll {
					r.invalid = append(r.invalid, "more than one installed event recovers enrollment")
				}
				r.caps.RecoverEnroll, r.caps.RecoverEvent = true, name
			}
			r.caps.PromptLine = r.caps.PromptLine || event.Fact == "prompt_submitted" && event.Notice &&
				(event.Output == "context" || event.Output == "inject_steps")
			r.caps.SessionEnd = r.caps.SessionEnd || event.Fact == "session_ended"
		}
		for event := range m.Install.Matchers {
			if !slices.Contains(m.Install.Events, event) {
				r.invalid = append(r.invalid, "matcher for "+event+", which is not installed")
			}
		}
		for revision, names := range m.PriorRevisions {
			if revision == m.Revision {
				r.invalid = append(r.invalid, "prior revision "+revision+" is the shipped one")
			}
			var caps Capabilities
			for _, name := range names {
				set, known := capabilityNames[name]
				if !known {
					r.invalid = append(r.invalid, revision+": unknown capability "+name)
					continue
				}
				set(&caps)
			}
			if caps.RecoverEnroll {
				caps.RecoverEvent = r.caps.RecoverEvent
			}
			r.prior[revision] = caps
		}
		all[adapter] = r
	}
	return all
}
