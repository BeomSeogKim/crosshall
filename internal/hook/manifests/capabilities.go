package manifests

import (
	"encoding/json"
	"sync"
)

// Capabilities are what an adapter's manifest declares that the core asks
// about. The core reads them here instead of testing adapter names, so an
// adapter gains a behaviour by declaring it.
type Capabilities struct {
	// TurnEnd: an installed event checks the end of a turn ("stop").
	TurnEnd bool
	// RecoverEnroll: an installed event may enroll a session that missed
	// its start ("recover_enroll").
	RecoverEnroll bool
}

// CapabilitiesOf returns the capabilities adapter's manifest declares; ok
// is false when the build has no readable manifest for it.
func CapabilitiesOf(adapter string) (caps Capabilities, ok bool) {
	caps, ok = capabilities()[adapter]
	return caps, ok
}

var capabilities = sync.OnceValue(func() map[string]Capabilities {
	all := make(map[string]Capabilities)
	for _, adapter := range Names() {
		raw, err := FS.ReadFile(adapter + ".json")
		if err != nil {
			continue
		}
		var m struct {
			Adapter string `json:"adapter"`
			Install struct {
				Events []string `json:"events"`
			} `json:"install"`
			Events map[string]struct {
				Stop          bool `json:"stop"`
				RecoverEnroll bool `json:"recover_enroll"`
			} `json:"events"`
		}
		if json.Unmarshal(raw, &m) != nil || m.Adapter != adapter {
			continue
		}
		var caps Capabilities
		for _, name := range m.Install.Events {
			event := m.Events[name]
			caps.TurnEnd = caps.TurnEnd || event.Stop
			caps.RecoverEnroll = caps.RecoverEnroll || event.RecoverEnroll
		}
		all[adapter] = caps
	}
	return all
})
