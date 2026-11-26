package link

import "encoding/json"

// CapEvents names the event stream: the home streams member-visible facts
// to a linked device, and the device forwards its sessions' hook facts
// and written offers (#400, design sections 4 and 6a). Both sides must
// list it for any frame below to be sent.
const CapEvents = "events"

// Frames of the event stream.
const (
	// Spoke to home.
	TypeSubscribe = "subscribe"
	TypeAck       = "ack"
	TypeFact      = "fact"
	TypeWritten   = "written"
	// Home to spoke.
	TypeEvent    = "event"
	TypeSnapshot = "snapshot"
	TypeCaughtUp = "caught_up"
	TypeRecorded = "recorded"
)

// Subscribe asks the home for every event after FromSeq, under the
// device's boot incarnation Boot. The home sends a Snapshot instead when
// Snapshot is set, when FromSeq is 0 or no longer kept, or when Boot is
// not the boot of the device's last acknowledged snapshot. A device sets
// Snapshot on its first subscribe after it starts. Either way the home
// then sends CaughtUp, also when there was nothing to send.
type Subscribe struct {
	FromSeq  uint64 `json:"from_seq"`
	Boot     string `json:"boot"`
	Snapshot bool   `json:"snapshot,omitempty"`
}

// CaughtUp says the home has sent every event up to Seq; later events
// follow as they happen.
type CaughtUp struct {
	Seq uint64 `json:"seq"`
}

// Ack says the device applied every event up to Seq durably, under its
// boot incarnation Boot; the home may trim them. Ack is only the home's
// cursor. Cuts carries, for each membership or invalidate event in that
// range, the member's leaf publish sequence at which the device applied
// it, under that leaf's publish lock: facts with a higher LeafSeq were
// captured after the change. The Ack of a Snapshot carries a cut for
// every member in it, the applied baseline after a resync.
//
// Cuts for one Seq may span several Ack frames, each within one link
// frame: every frame but the last sets More, and the home applies the
// cuts of a Seq only once it has the frame without More.
type Ack struct {
	Seq  uint64    `json:"seq"`
	Boot string    `json:"boot"`
	Cuts []LeafCut `json:"cuts,omitempty"`
	More bool      `json:"more,omitempty"`
}

// LeafCut is the leaf publish sequence at which the device applied the
// event Seq to Member, with the scope that event carried. The device cuts
// only a leaf whose current generation is Scope.LeafGeneration; the home
// installs a cut only when Scope equals the scope it recorded for that
// event, so a cut never moves to another registration of the same key.
type LeafCut struct {
	Member
	Seq     uint64      `json:"seq"`
	LeafSeq uint64      `json:"leaf_seq"`
	Scope   MemberScope `json:"scope"`
}

// MemberScope is what a membership-changing event applies to: the leaf
// registration, the selected membership and the route epoch the home
// recorded. A same-binding rejoin keeps the membership generation but
// not the route epoch or leaf generation.
type MemberScope struct {
	LeafGeneration string     `json:"leaf_generation"`
	Membership     Membership `json:"membership"`
	RouteEpoch     int64      `json:"route_epoch"`
}

// Event is one member-visible fact for a member on the device, in the
// home's per-device order.
type Event struct {
	Seq  uint64          `json:"seq"`
	Kind string          `json:"kind"`
	Body json.RawMessage `json:"body"`
}

// Event kinds.
const (
	EventOffer      = "offer"
	EventInvalidate = "invalidate"
	EventMembership = "membership"
)

// Member names a member on the device by the session key its dispatcher
// authenticates; the device itself comes from the link.
type Member struct {
	Vendor string `json:"vendor"`
	Key    string `json:"key"`
}

// Membership is a member's selected binding and its generation, as the
// home's member_selection holds them.
type Membership struct {
	BindingID  int64 `json:"binding_id"`
	Generation int64 `json:"generation"`
}

// Offer is a notice the home prepared for a member, with the identity the
// hook must match exactly before it may show it: the ordered batch, the
// binding and its route and policy epochs, the launch and its epoch, and
// the nonce its written acknowledgement must carry, and the leaf
// generation and membership selection it was prepared for.
//
// LeaseMS is the offer's remaining lifetime when the home sent this frame,
// computed again for every send. The device keeps the earlier of its
// current deadline and its monotonic receive time plus LeaseMS, so a
// replay never extends an offer; zero or less retires it.
type Offer struct {
	Member
	OfferID        int64      `json:"offer_id"`
	Nonce          string     `json:"nonce"`
	LeafGeneration string     `json:"leaf_generation"`
	Membership     Membership `json:"membership"`
	BindingID      int64      `json:"binding_id"`
	RouteEpoch     int64      `json:"route_epoch"`
	PolicyEpoch    int64      `json:"policy_epoch"`
	Launch         string     `json:"launch,omitempty"`
	Epoch          int64      `json:"epoch"`
	IDs            []int64    `json:"ids"`
	LeaseMS        int64      `json:"lease_ms"`
}

// Invalidate retires what the device holds for a member before any newer
// event: its offers, and its membership stamp when Reason says so. Scope
// is the member's scope after the change.
type Invalidate struct {
	Member
	Reason string      `json:"reason"`
	Scope  MemberScope `json:"scope"`
}

// Invalidation reasons.
const (
	InvalidateMove     = "move"
	InvalidateDepart   = "depart"
	InvalidateDND      = "dnd"
	InvalidatePolicy   = "policy"
	InvalidateRoute    = "route"
	InvalidateCancel   = "cancel"
	InvalidateOfferSet = "offer_set"
)

// MembershipChange carries a member's current scope.
type MembershipChange struct {
	Member
	Scope MemberScope `json:"scope"`
}

// Snapshot replaces everything the device holds: every member's current
// scope and every offer prepared and not retired as of Seq. Events after
// Seq follow it.
//
// A snapshot spans one or more frames with the same Seq and Part 0, 1, 2
// and so on, each within one link frame; the frame with Last ends it.
// The device applies it atomically once it has every part, dropping its
// cache first; a link that ends before Last leaves the cache as it was.
type Snapshot struct {
	Seq     uint64           `json:"seq"`
	Part    int              `json:"part"`
	Last    bool             `json:"last,omitempty"`
	Members []SnapshotMember `json:"members"`
}

// SnapshotMember is one member's state in a snapshot.
type SnapshotMember struct {
	Member
	Scope  MemberScope `json:"scope"`
	Offers []Offer     `json:"offers,omitempty"`
}

// HookToken is the causal token a hook takes at capture, under its leaf's
// publish lock (design 6a): the device's boot incarnation, the leaf's
// publish sequence, the monotonic and wall capture times, and the leaf
// generation and membership the hook saw. The home orders and fences a
// device's facts by Boot and LeafSeq; times are for display only.
type HookToken struct {
	Boot           string     `json:"boot"`
	LeafSeq        uint64     `json:"leaf_seq"`
	MonoNS         int64      `json:"mono_ns"`
	WallMS         int64      `json:"wall_ms"`
	LeafGeneration string     `json:"leaf_generation"`
	Membership     Membership `json:"membership"`
}

// Fact is a hook fact of a member's session, forwarded with its token.
// ID is the device's outbox entry; the device retires it only on a
// Recorded with that ID, never on the socket write.
//
// Kind is the local hook verb, wire.VerbHookEnroll or wire.VerbHookEvent,
// and Body its arguments, wire.HookEnrollArgs or wire.HookEventArgs, as
// the hook sent them to its device's dispatcher. The device judges the
// process itself and sends its judgment as LaunchVerified and
// LaunchHosted; it clears what names only the device: PIDNamespace, Host,
// VendorHome and every repository path. The home identifies the host as
// the link's device and the token's Boot, and never probes a process.
type Fact struct {
	ID uint64 `json:"id"`
	Member
	Token HookToken       `json:"token"`
	Kind  string          `json:"kind"`
	Body  json.RawMessage `json:"body,omitempty"`
}

// Written acknowledges that a hook wrote an offer completely to its
// session, as the local post-write acknowledgement does. ID is as in
// Fact.
type Written struct {
	ID uint64 `json:"id"`
	Member
	OfferID     int64     `json:"offer_id"`
	Nonce       string    `json:"nonce"`
	WrittenAtMS int64     `json:"written_at_ms"`
	Token       HookToken `json:"token"`
}

// Recorded is the home's durable result for one Fact or Written, by its
// ID, sent after the home's transaction committed.
type Recorded struct {
	ID     uint64 `json:"id"`
	Status string `json:"status"`
	Code   string `json:"code,omitempty"`
}

// Recorded statuses.
const (
	// RecordedApplied: the home applied it.
	RecordedApplied = "applied"
	// RecordedStale: its token is behind the home's fence; it changed
	// nothing and will never apply.
	RecordedStale = "stale"
	// RecordedQuarantined: the home kept it without applying it until the
	// device's cut or baseline orders it.
	RecordedQuarantined = "quarantined"
)
