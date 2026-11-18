// Package wire defines the versioned JSON envelopes exchanged through spools.
package wire

import (
	"encoding/json"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

// Version identifies the request and response envelope format.
const Version = 1

// Request frames one attempt. Owner requests omit Generation and KeyDigest.
type Request struct {
	Version       int             `json:"v"`
	Attempt       string          `json:"attempt"`
	OpKey         string          `json:"op_key"`
	Generation    string          `json:"gen,omitempty"`
	KeyDigest     string          `json:"kd,omitempty"`
	VendorVersion string          `json:"vendor_version,omitempty"`
	IssuedAtMS    int64           `json:"issued_at_ms"`
	ExpiresAtMS   int64           `json:"expires_at_ms"`
	Ack           uint64          `json:"ack"`
	Verb          string          `json:"verb"`
	Args          json.RawMessage `json:"args"`
}

// Response correlates a command result with the attempt that produced it.
type Response struct {
	Version int    `json:"v"`
	Attempt string `json:"attempt"`
	core.Result
}
