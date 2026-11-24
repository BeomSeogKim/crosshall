// First contact uses its own attempt and cannot carry a domain command.
package wire

import (
	"crypto/rand"
	"encoding/hex"
)

// Hello asks the dispatcher to find or prepare a member's exchange directory.
type Hello struct {
	Version       int    `json:"v"`
	Attempt       string `json:"attempt"`
	Vendor        string `json:"vendor"`
	Key           string `json:"key"`
	VendorVersion string `json:"vendor_version,omitempty"`
	Intent        string `json:"intent"`
}

// NewID returns a fresh, filename-safe 128-bit identifier.
func NewID() string {
	var id [16]byte
	rand.Read(id[:])
	return hex.EncodeToString(id[:])
}

// HelloStart is the first-contact intent of a session start that will join
// if its key is unknown: it registers an unknown key, as join does, and
// refuses a retired key, as lookup does. A dispatcher lists it in its ready
// marker's hello field when it accepts it.
const HelloStart = "start"
