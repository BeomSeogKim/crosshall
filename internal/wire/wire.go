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
	Membership    *Membership     `json:"membership,omitempty"`
	KeyDigest     string          `json:"kd,omitempty"`
	VendorVersion string          `json:"vendor_version,omitempty"`
	IssuedAtMS    int64           `json:"issued_at_ms"`
	ExpiresAtMS   int64           `json:"expires_at_ms"`
	PublishSeq    uint64          `json:"publish_seq,omitempty"`
	Ack           uint64          `json:"ack"`
	Verb          string          `json:"verb"`
	Args          json.RawMessage `json:"args"`
	// Reply defaults to true. Only member observations may request no reply.
	Reply *bool `json:"reply,omitempty"`
	// Host is the agent process that runs a member command, when the
	// client could identify it.
	Host *Host `json:"host,omitempty"`
	// OwnerAlerts says the client prints the owner line of an owner
	// request; only such a request marks the stops it carries as shown.
	OwnerAlerts bool `json:"owner_alerts,omitempty"`
	// Older dispatchers ignore envelope extensions but reject unknown command arguments.
	OfferReceipts bool `json:"offer_receipts,omitempty"`
	// Capture is the causal token a hook takes under its leaf's publish
	// lock, with PublishSeq, when its dispatcher is a linked device's
	// (#400, design 6a). Native hooks leave it out.
	Capture *HookCapture `json:"capture,omitempty"`
}

// HookCapture is what a hook reads under its leaf's publish lock: the
// device's boot incarnation, monotonic and wall capture times, and the
// leaf's membership stamp at that moment.
type HookCapture struct {
	Boot       string      `json:"boot"`
	MonoNS     int64       `json:"mono_ns"`
	WallMS     int64       `json:"wall_ms"`
	Membership *Membership `json:"membership,omitempty"`
}

type Membership = core.Membership

// Host names a process by its PID, its start identity and the PID
// namespace the PID belongs to.
type Host struct {
	PID   int64  `json:"pid"`
	Start int64  `json:"start"`
	NS    string `json:"ns,omitempty"`
}

// Response correlates a command result with the attempt that produced it.
type Response struct {
	Version int    `json:"v"`
	Attempt string `json:"attempt"`
	core.Result
}

const (
	VerbOwnerReady = "owner ready"
	VerbOwnerStop  = "owner stop"
)

// DispatcherInstance binds an owner control request and reply to one daemon.
type DispatcherInstance struct {
	Instance string `json:"instance"`
}
