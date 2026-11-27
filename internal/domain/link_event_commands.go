package domain

import (
	"context"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/link"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

const (
	VerbLinkEvents  = "owner link events"
	VerbLinkAck     = "owner link ack"
	VerbLinkFact    = "owner link fact"
	VerbLinkWritten = "owner link written"
)

type LinkEventPeer struct {
	Device        string `json:"device"`
	CredentialGen int64  `json:"credential_gen"`
	Admission     int64  `json:"admission"`
	Stream        string `json:"stream"`
	// ThreadTurnEnd: the device negotiated link.CapThreadTurnEnd, so it
	// captures a thread's route at a turn end that names its server.
	ThreadTurnEnd bool `json:"thread_turn_end,omitempty"`
}

type LinkEvents struct {
	LinkEventPeer
	AllowWake    bool            `json:"allow_wake,omitempty"`
	AllowPolicy  bool            `json:"allow_policy,omitempty"`
	Resubscribe  bool            `json:"resubscribe,omitempty"`
	Close        bool            `json:"close,omitempty"`
	Subscribe    *link.Subscribe `json:"subscribe,omitempty"`
	FromSeq      uint64          `json:"from_seq,omitempty"`
	SnapshotPart *int            `json:"snapshot_part,omitempty"`
}

type LinkEventsResult struct {
	Events   []link.Event   `json:"events,omitempty"`
	Snapshot *link.Snapshot `json:"snapshot,omitempty"`
	Through  uint64         `json:"through"`
}

type LinkAck struct {
	LinkEventPeer
	Ack link.Ack `json:"ack"`
}

type LinkFact struct {
	LinkEventPeer
	Fact link.Fact `json:"fact"`
}

type LinkWritten struct {
	LinkEventPeer
	Written link.Written `json:"written"`
}

func (LinkEvents) command()  {}
func (LinkAck) command()     {}
func (LinkFact) command()    {}
func (LinkWritten) command() {}

func (LinkEvents) Verb() string  { return VerbLinkEvents }
func (LinkAck) Verb() string     { return VerbLinkAck }
func (LinkFact) Verb() string    { return VerbLinkFact }
func (LinkWritten) Verb() string { return VerbLinkWritten }

func authorizeLinkEventPeer(ctx context.Context, tx store.Tx, caller core.Caller, peer LinkEventPeer) (*core.Result, error) {
	if caller.Role != core.RoleOwner {
		result := linkOwnerOnly()
		return &result, nil
	}
	if !link.ValidNonce(peer.Stream) {
		result := Fail(core.ExitUsage, "", "invalid link event stream", "reconnect the link")
		return &result, nil
	}
	return linkCredential(ctx, tx, LinkCall{Device: peer.Device, CredentialGen: peer.CredentialGen, Admission: peer.Admission})
}
