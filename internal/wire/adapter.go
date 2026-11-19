package wire

import "encoding/json"

const (
	VerbHookEnroll = "hook.enroll"
	VerbHookEvent  = "hook.event"
	VerbAdapterSet = "owner adapter set"
)

const (
	HookPromptSubmitted   = "prompt_submitted"
	HookTurnFinishing     = "turn_finishing"
	HookApprovalRequested = "approval_requested"
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
	EventRef       string      `json:"event_ref"`
	Source         string      `json:"source"`
	RepoRoot       string      `json:"repo_root"`
	HookAtMS       int64       `json:"hook_at_ms"`
	Epoch          uint64      `json:"epoch"`
}

type HookEventArgs struct {
	Profile        HookProfile `json:"profile"`
	LaunchRef      string      `json:"launch_ref"`
	LaunchVerified bool        `json:"launch_verified"`
	EventRef       string      `json:"event_ref"`
	Fact           string      `json:"fact"`
	TurnRef        string      `json:"turn_ref,omitempty"`
	HookAtMS       int64       `json:"hook_at_ms"`
	WantOutput     bool        `json:"want_output"`
	Epoch          uint64      `json:"epoch"`
	Receipt        string      `json:"receipt,omitempty"`
	ToolCallID     string      `json:"tool_call_id,omitempty"`
	// ContinuationActive reports that a vendor Stop hook is already continuing the turn.
	ContinuationActive bool `json:"continuation_active,omitempty"`
}

// HookResult contains only presentation owned by the core. An acknowledgement
// is scoped to one output path and is independent of the roster cursor.
type HookResult struct {
	Enrollment   string          `json:"enrollment,omitempty"`
	Alias        string          `json:"alias,omitempty"`
	Channel      string          `json:"channel,omitempty"`
	Hint         string          `json:"hint,omitempty"`
	Roster       json.RawMessage `json:"roster,omitempty"`
	Omitted      int             `json:"omitted,omitempty"`
	OfferID      int64           `json:"offer_id,omitempty"`
	PendingLine  string          `json:"pending_line,omitempty"`
	TurnEndLine  string          `json:"turn_end_line,omitempty"`
	ReminderText string          `json:"reminder_text,omitempty"`
	Ack          *HookAck        `json:"ack,omitempty"`
	Epoch        uint64          `json:"epoch"`
}

type HookAck struct {
	Kind       string `json:"kind"`
	Instance   string `json:"instance"`
	Generation string `json:"gen"`
	Seq        uint64 `json:"seq"`
	TurnRef    string `json:"turn_ref,omitempty"`
}

type AdapterPolicyArgs struct {
	Adapter  string `json:"adapter"`
	Enabled  *bool  `json:"enabled,omitempty"`
	Reminder *bool  `json:"reminder,omitempty"`
}

func (r Request) IsHook() bool {
	return r.Verb == VerbHookEnroll || r.Verb == VerbHookEvent
}

// OneWayHook limits the no-reply envelope to an observation without output.
func (r Request) OneWayHook() bool {
	if r.Reply == nil || *r.Reply || r.Verb != VerbHookEvent {
		return false
	}
	var event HookEventArgs
	return json.Unmarshal(r.Args, &event) == nil && !event.WantOutput
}
