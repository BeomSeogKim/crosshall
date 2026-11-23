// Package telemetry builds the optional daily usage report: rounded counts
// under the keys of a compiled-in registry, and a few fixed enums, for one
// closed UTC day. Nothing in a report links one day to another, and no text
// from a message, channel, alias, path or vendor can be encoded in it.
package telemetry

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

// Report is one day's report in its canonical member order.
type Report struct {
	V        int              `json:"v"`
	Registry int              `json:"registry"`
	Batch    string           `json:"batch"`
	Day      string           `json:"day"`
	App      App              `json:"app"`
	Sources  Sources          `json:"sources"`
	Switches *Switches        `json:"switches,omitempty"`
	Counts   map[string]int64 `json:"counts"`
}

// App names the release and platform that built the report.
type App struct {
	Version  string `json:"version"`
	Platform string `json:"platform"`
}

// Sources says which inputs a report had.
type Sources struct {
	Hooks    string `json:"hooks"`
	Snapshot string `json:"snapshot"`
}

// Switches are the two owner switches at the day's close, sent only with a
// snapshot.
type Switches struct {
	PublishHall   string `json:"publish_hall"`
	TurnEndClaude string `json:"turn_end_claude"`
}

// Marshal returns the canonical bytes: no whitespace, members in the order
// of the wire contract, count keys in ascending byte order.
func (r Report) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(r); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

var (
	releasePattern = regexp.MustCompile(`^v[0-9]{1,2}\.[0-9]{1,2}\.[0-9]{1,2}(-rc\.[0-9]{1,2})?$`)
	batchPattern   = regexp.MustCompile(`^[0-9a-f]{32}$`)
	familyPattern  = regexp.MustCompile(`^([0-9]{1,4})\.([0-9]{1,6})(\.[0-9]{1,6}){0,2}$`)
)

// AppVersion returns version when it is a release tag, otherwise "dev",
// which validation rejects: a development build never reports.
func AppVersion(version string) string {
	if releasePattern.MatchString(version) {
		return version
	}
	return "dev"
}

// Round rounds a count down to the value ladder; below 1 it is 0, which is
// never sent.
func Round(n int64) int64 {
	var out int64
	for _, v := range registry.Values {
		if n >= v {
			out = v
		}
	}
	return out
}

// family maps an agent's runtime version to its registry family: the major
// and minor number when the registry lists them, "unknown" for an unverified
// or absent version, and "other" for anything else.
func family(adapter, version string) string {
	if version == "" || version == "unverified" || version == "unknown" {
		return "unknown"
	}
	prefix := ""
	if adapter == "agy" {
		// Antigravity reports its language server's build.
		prefix = "ls_"
		version = strings.TrimPrefix(version, "ls:")
	}
	if m := familyPattern.FindStringSubmatch(version); m != nil {
		if f := prefix + m[1] + "_" + m[2]; registry.has("snap.live_launch." + adapter + "." + f) {
			return f
		}
	}
	return "other"
}
