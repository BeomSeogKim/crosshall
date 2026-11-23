package telemetry

import (
	_ "embed"
	"encoding/json"
	"slices"
)

// registryJSON is the contract shared with the receiver: every key and enum
// value a report may carry. Changing it means a new registry number.
//
//go:embed registry.json
var registryJSON []byte

type registryFile struct {
	Registry  int                 `json:"registry"`
	Values    []int64             `json:"values"`
	Platforms []string            `json:"platforms"`
	Sources   map[string][]string `json:"sources"`
	Switches  map[string][]string `json:"switches"`
	Keys      []string            `json:"keys"`
	keys      map[string]bool
}

func (r *registryFile) has(key string) bool { return r.keys[key] }

var registry = func() *registryFile {
	var r registryFile
	if err := json.Unmarshal(registryJSON, &r); err != nil {
		panic("telemetry: registry.json: " + err.Error())
	}
	r.keys = make(map[string]bool, len(r.Keys))
	for _, k := range r.Keys {
		r.keys[k] = true
	}
	return &r
}()

// Keys returns the registry's count keys in ascending byte order.
func Keys() []string { return slices.Clone(registry.Keys) }

// Dimensions the builder composes keys from. A test checks that every key
// they can make is in the registry and the other way round.
var (
	adapters = []string{"claude", "codex", "agy"}
	// hookClasses maps a manifest event to its class; any other event is
	// "other".
	hookClasses = map[string]string{
		"SessionStart": "start", "UserPromptSubmit": "prompt", "Stop": "stop", "SessionEnd": "end",
	}
	classes       = []string{"start", "prompt", "stop", "end", "other"}
	hookOutcomes  = []string{"off", "unmatched", "identity", "clean", "rpc", "deadline", "refused", "stale_epoch", "no_dispatcher"}
	hookMSBuckets = []string{"lt5", "lt10", "lt28", "lt50", "lt150", "ge150"}
	hookMSBounds  = []int{5, 10, 28, 50, 150}
	pickupBuckets = []string{"lt10s", "lt1m", "lt10m", "lt1h", "lt1d", "ge1d"}
	pickupBounds  = []int64{10_000, 60_000, 600_000, 3_600_000, 86_400_000}
	messageKinds  = []string{"ask", "reply", "note"}
	settlements   = []string{"replied", "declined", "cancelled"}
	resolutions   = []string{"submitted", "not_submitted", "uncertain"}
	observations  = []string{"own_turn", "absorbed", "unknown"}
	rejections    = []string{"malformed", "stale"}
	suspensions   = []string{"absorbed", "unknown", "not_submitted"}
	reachStates   = []string{"off", "suspended", "capped", "held", "candidate", "available"}
	// families lists each adapter's version families besides "other" and
	// "unknown".
	families = map[string][]string{"claude": {"2_1"}, "codex": {"0_170", "0_171"}, "agy": {"ls_1_5"}}
)

func bucket(names []string, bounds []int64, v int64) string {
	for i, b := range bounds {
		if v < b {
			return names[i]
		}
	}
	return names[len(names)-1]
}
