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
