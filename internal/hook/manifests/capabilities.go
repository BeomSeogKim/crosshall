package manifests

import (
	"encoding/json"
	"sync"
)

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
	// SharedDaemon: the vendor can run sessions in a shared managed server
	// ("daemon"), whose hooks inherit that server's environment.
	SharedDaemon bool
}

// capabilityNames are the names a manifest's prior_revisions may list.
var capabilityNames = map[string]func(*Capabilities){
	"process_launch": func(c *Capabilities) { c.ProcessLaunch = true },
	"turn_end":       func(c *Capabilities) { c.TurnEnd = true },
	"recover_enroll": func(c *Capabilities) { c.RecoverEnroll = true },
	"shared_daemon":  func(c *Capabilities) { c.SharedDaemon = true },
}

type record struct {
	revision string
	caps     Capabilities
	prior    map[string]Capabilities
	display  string
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

var records = sync.OnceValue(func() map[string]record {
	all := make(map[string]record)
	for _, adapter := range Names() {
		raw, err := FS.ReadFile(adapter + ".json")
		if err != nil {
			continue
		}
		var m struct {
			Adapter        string              `json:"adapter"`
			Revision       string              `json:"revision"`
			DisplayName    string              `json:"display_name"`
			PriorRevisions map[string][]string `json:"prior_revisions"`
			LaunchEnv      string              `json:"launch_env"`
			Daemon         json.RawMessage     `json:"daemon"`
			Install        struct {
				Events []string `json:"events"`
			} `json:"install"`
			Events map[string]struct {
				Stop          bool `json:"stop"`
				RecoverEnroll bool `json:"recover_enroll"`
			} `json:"events"`
		}
		if json.Unmarshal(raw, &m) != nil || m.Adapter != adapter || m.Revision == "" {
			continue
		}
		r := record{revision: m.Revision, display: m.DisplayName, prior: make(map[string]Capabilities)}
		r.caps.ProcessLaunch = m.LaunchEnv != ""
		r.caps.SharedDaemon = len(m.Daemon) > 0 && string(m.Daemon) != "null"
		for _, name := range m.Install.Events {
			event := m.Events[name]
			r.caps.TurnEnd = r.caps.TurnEnd || event.Stop
			r.caps.RecoverEnroll = r.caps.RecoverEnroll || event.RecoverEnroll
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
			r.prior[revision] = caps
		}
		all[adapter] = r
	}
	return all
})
