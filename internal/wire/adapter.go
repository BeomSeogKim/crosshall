package wire

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
)

const (
	VerbHookEnroll      = "hook.enroll"
	VerbHookEvent       = "hook.event"
	VerbHookRoute       = "hook.route"
	VerbAdapterSet      = "owner adapter set"
	VerbHookJoinHintAck = "hook.join_hint_ack"
)

const (
	HookPromptSubmitted   = "prompt_submitted"
	HookTurnFinishing     = "turn_finishing"
	HookApprovalRequested = "approval_requested"
	HookIdlePrompt        = "idle_prompt"
	HookSessionEnded      = "session_ended"
	HookPickupSeen        = "pickup_seen"
)

type HookProfile struct {
	Adapter       string `json:"adapter"`
	ManifestRev   string `json:"manifest_rev"`
	VendorVersion string `json:"vendor_version"`
	Platform      string `json:"platform"`
	Mode          string `json:"mode"`
}

type HookEnrollArgs struct {
	Profile        HookProfile `json:"profile"`
	LaunchRef      string      `json:"launch_ref"`
	LaunchVerified bool        `json:"launch_verified"`
	// LaunchHosted says the hook's nearest non-shell ancestor is the launch's
	// process, so the start is that process's own and not a nested session's.
	LaunchHosted bool   `json:"launch_hosted,omitempty"`
	PIDNamespace string `json:"pid_namespace,omitempty"`
	// Host is the managed server that runs the hook, for a vendor whose
	// sessions are its clients (Profile.Mode daemon): it holds the
	// session's thread.
	Host *Host `json:"host,omitempty"`
	// VendorHome is the vendor home that server serves, with symbolic links
	// resolved, sent with Host.
	VendorHome string `json:"vendor_home,omitempty"`
	EventRef   string `json:"event_ref"`
	Source     string `json:"source"`
	RepoRoot   string `json:"repo_root"`
	// Repository names the whole repository of RepoRoot's worktree, and
	// CreateChannel says the hook found a real work tree there, so the
	// repository may get its own channel. The dispatcher resolves the
	// worktree's default, then the repository's, in its transaction.
	Repository    string `json:"repository,omitempty"`
	CreateChannel bool   `json:"create_channel,omitempty"`
	HookAtMS      int64  `json:"hook_at_ms"`
	Epoch         uint64 `json:"epoch"`
}

type HookEventArgs struct {
	Profile          HookProfile `json:"profile"`
	LaunchRef        string      `json:"launch_ref"`
	LaunchVerified   bool        `json:"launch_verified"`
	EventRef         string      `json:"event_ref"`
	Fact             string      `json:"fact"`
	TurnRef          string      `json:"turn_ref,omitempty"`
	HookAtMS         int64       `json:"hook_at_ms"`
	WantOutput       bool        `json:"want_output"`
	Epoch            uint64      `json:"epoch"`
	EnrollRepoRoot   string      `json:"enroll_repo_root,omitempty"`
	EnrollRepository string      `json:"enroll_repository,omitempty"`
	EnrollCreate     bool        `json:"enroll_create,omitempty"`
	// Receipt records an observed publish notice as "publish:" followed by its IDs.
	Receipt    string `json:"receipt,omitempty"`
	ToolCallID string `json:"tool_call_id,omitempty"`
	// ContinuationActive reports that a vendor Stop hook is already continuing the turn.
	ContinuationActive bool `json:"continuation_active,omitempty"`
	// LaunchAtMS and PIDNamespace come with a prompt of a verified process
	// launch: the time its record was written and the hook's PID namespace,
	// so a launch whose start the dispatcher never saw can be bound late.
	LaunchAtMS   int64  `json:"launch_at_ms,omitempty"`
	PIDNamespace string `json:"pid_namespace,omitempty"`
	// Host and VendorHome come with a prompt as with a start (see
	// HookEnrollArgs), so a session whose start the dispatcher never saw
	// gets the notice route that start would have registered.
	Host       *Host  `json:"host,omitempty"`
	VendorHome string `json:"vendor_home,omitempty"`
}

// CanRecoverEnrollment: a prompt callback may enroll a session that missed
// its start when the exact revision of its profile declares recovery and
// the callback is that revision's recovering event. A revision this build
// does not know recovers nothing.
func (a HookEventArgs) CanRecoverEnrollment() bool {
	if a.EnrollRepoRoot == "" || !a.WantOutput || a.Fact != HookPromptSubmitted || a.TurnRef == "" {
		return false
	}
	caps, _ := manifests.CapabilitiesAt(a.Profile.Adapter, a.Profile.ManifestRev)
	ref := a.TurnRef + ":" + caps.RecoverEvent
	return caps.RecoverEnroll && (a.EventRef == ref || a.EventRef == PromptEventRef(ref, a.HookAtMS))
}

// PromptEventRef is the reference of a prompt callback: its event's
// reference and the hook's entry time. A prompt typed while the previous
// turn runs reports that turn's id (Claude Code, #163); the entry time tells
// the two callbacks apart, and a retry of the same hook keeps it. A
// reference that would grow past 128 bytes is replaced by its digest.
func PromptEventRef(ref string, entryMS int64) string {
	ref += "@" + strconv.FormatInt(entryMS, 10)
	if len(ref) > 128 {
		return fmt.Sprintf("prompt:%x", sha256.Sum256([]byte(ref)))
	}
	return ref
}

type HookRouteArgs struct {
	Adapter        string `json:"adapter"`
	LaunchRef      string `json:"launch_ref"`
	LaunchVerified bool   `json:"launch_verified"`
	EventRef       string `json:"event_ref"`
	HookAtMS       int64  `json:"hook_at_ms"`
	Epoch          uint64 `json:"epoch"`
	PermissionMode string `json:"permission_mode,omitempty"`
	SocketPath     string `json:"socket_path,omitempty"`
	// Start marks a session start's report: only a start replaces a
	// different socket path, and it records whether it saw one.
	Start bool `json:"start,omitempty"`
}

// HookResult contains only presentation owned by the core. An acknowledgement
// is scoped to one output path and is independent of the roster cursor. Cue is
// the short text for a restored session whose context was reset.
type HookResult struct {
	Enrollment string `json:"enrollment,omitempty"`
	Alias      string `json:"alias,omitempty"`
	Channel    string `json:"channel,omitempty"`
	Hint       string `json:"hint,omitempty"`
	Cue        string `json:"cue,omitempty"`
	// CueWithStartLine replaces Cue in an output that also prints the start
	// line (Hint), which already says who the session is.
	CueWithStartLine *string           `json:"cue_with_start_line,omitempty"`
	Roster           json.RawMessage   `json:"roster,omitempty"`
	Omitted          int               `json:"omitted,omitempty"`
	OfferID          int64             `json:"offer_id,omitempty"`
	OfferReceipt     *HookOfferReceipt `json:"offer_receipt,omitempty"`
	PendingLine      string            `json:"pending_line,omitempty"`
	TurnEndLine      string            `json:"turn_end_line,omitempty"`
	// ReachLine says whether the hall brings new work to the session;
	// ReachAck stores it as told once the hook has printed it.
	ReachLine    string           `json:"reach_line,omitempty"`
	ReachAck     *HookReachAck    `json:"reach_ack,omitempty"`
	ReminderText string           `json:"reminder_text,omitempty"`
	Ack          *HookAck         `json:"ack,omitempty"`
	JoinHintAck  *HookJoinHintAck `json:"join_hint_ack,omitempty"`
	// ChannelCreated tells the dispatcher that this enrolment made the
	// repository's channel, so the published repository set is stale.
	ChannelCreated bool   `json:"channel_created,omitempty"`
	Epoch          uint64 `json:"epoch"`
}

// HookOfferReceipt identifies a prepared batch for acknowledgement after output.
type HookOfferReceipt struct {
	OfferID int64  `json:"offer_id"`
	Nonce   string `json:"nonce"`
}

type HookOutputAckArgs struct {
	Offer       *HookOfferReceipt `json:"offer,omitempty"`
	WrittenAtMS int64             `json:"written_at_ms,omitempty"`
}

type HookJoinHintAck struct {
	BindingID            int64  `json:"binding_id"`
	RouteEpoch           int64  `json:"route_epoch"`
	Generation           string `json:"gen"`
	RosterSeq            uint64 `json:"roster_seq"`
	Offer                string `json:"offer,omitempty"`
	MembershipGeneration int64  `json:"membership_generation,omitempty"`
}

func (a HookJoinHintAck) ValidFor(generation string) bool {
	return generation != "" && a.Generation == generation &&
		a.BindingID > 0 && a.RouteEpoch > 0 && int64(a.RosterSeq) > 0 && a.Offer != ""
}

func (a HookJoinHintAck) SameOffer(b HookJoinHintAck) bool {
	return a.Offer != "" && a.Offer == b.Offer && a.BindingID == b.BindingID &&
		a.RouteEpoch == b.RouteEpoch && a.Generation == b.Generation && a.MembershipGeneration == b.MembershipGeneration
}

// HookReachAck identifies a reach line offered to a binding: Told is the
// value stored once the line was printed (1 brings, 0 cannot reach).
type HookReachAck struct {
	BindingID            int64  `json:"binding_id"`
	RouteEpoch           int64  `json:"route_epoch"`
	Told                 int    `json:"told"`
	SuspensionID         int64  `json:"suspension_id,omitempty"`
	Generation           string `json:"gen"`
	Offer                string `json:"offer,omitempty"`
	MembershipGeneration int64  `json:"membership_generation,omitempty"`
}

func (a HookReachAck) ValidFor(generation string) bool {
	return generation != "" && a.Generation == generation && a.BindingID > 0 && a.RouteEpoch > 0 &&
		(a.Told == 0 || a.Told == 1) && a.SuspensionID >= 0 && (a.SuspensionID == 0 || a.Told == 0) && a.Offer != ""
}

func (a HookReachAck) SameOffer(b HookReachAck) bool {
	return a.Offer != "" && a == b
}

type HookAck struct {
	Kind       string     `json:"kind"`
	Instance   string     `json:"instance"`
	Generation string     `json:"gen"`
	Seq        uint64     `json:"seq"`
	TurnRef    string     `json:"turn_ref,omitempty"`
	Membership Membership `json:"membership,omitzero"`
}

type AdapterPolicyArgs struct {
	Adapter  string `json:"adapter"`
	Enabled  *bool  `json:"enabled,omitempty"`
	Reminder *bool  `json:"reminder,omitempty"`
	// Publish is on, off, or default (the hall switch decides).
	Publish *string `json:"publish,omitempty"`
	// TurnEnd is on, off, or default (the build's evidence decides).
	TurnEnd *string `json:"turn_end,omitempty"`
}

func (r Request) IsHook() bool {
	return r.Verb == VerbHookEnroll || r.Verb == VerbHookEvent || r.Verb == VerbHookRoute || r.Verb == VerbHookJoinHintAck
}

// OneWayHook limits the no-reply envelope to an observation without output.
func (r Request) OneWayHook() bool {
	if r.Reply == nil || *r.Reply {
		return false
	}
	if r.Verb == VerbHookRoute || r.Verb == VerbHookJoinHintAck {
		return true
	}
	if r.Verb != VerbHookEvent {
		return false
	}
	var event HookEventArgs
	return json.Unmarshal(r.Args, &event) == nil && !event.WantOutput
}
