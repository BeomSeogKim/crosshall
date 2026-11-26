// First contact uses its own attempt and cannot carry a domain command.
package wire

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// Hello asks the dispatcher to find or prepare a member's exchange directory.
type Hello struct {
	Version       int    `json:"v"`
	Attempt       string `json:"attempt"`
	Vendor        string `json:"vendor"`
	Key           string `json:"key"`
	VendorVersion string `json:"vendor_version,omitempty"`
	Intent        string `json:"intent"`
	ExpiresAtMS   int64  `json:"expires_at_ms,omitempty"`
}

// NewID returns a fresh, filename-safe 128-bit identifier.
func NewID() string {
	var id [16]byte
	rand.Read(id[:])
	return hex.EncodeToString(id[:])
}

const (
	WaitAttemptPrefix = "wait:"
	WaitRetryPrefix   = "wait-retry:"
)

// WaitAttempt recognizes the invocation ID carried in the existing key
// field so older dispatchers can still decode the request.
func WaitAttempt(key string) (id string, retry bool) {
	id, retry = strings.CutPrefix(key, WaitRetryPrefix)
	if !retry {
		var found bool
		id, found = strings.CutPrefix(key, WaitAttemptPrefix)
		if !found {
			return "", false
		}
	}
	if len(id) != 32 {
		return "", false
	}
	if _, err := hex.DecodeString(id); err != nil {
		return "", false
	}
	return id, retry
}

// HelloStart is the first-contact intent of a session start that will join
// if its key is unknown: it registers an unknown key, as join does, and
// refuses a retired key, as lookup does. A dispatcher lists it in its ready
// marker's hello field when it accepts it.
const HelloStart = "start"

// HelloLaunchProof is listed in the ready marker's hello field by a
// dispatcher that accepts a prompt's launch proof (launch_at_ms and
// pid_namespace in hook.event): an older one refuses arguments it does not
// know, so a hook sends the proof only when it is listed.
const HelloLaunchProof = "launch_proof"

// HelloPromptHost is listed by a dispatcher that accepts a prompt's server
// (host and vendor_home in hook.event), which registers the notice route of
// a session whose start it missed (#252); a hook sends them only when it is
// listed.
const HelloPromptHost = "prompt_host"
