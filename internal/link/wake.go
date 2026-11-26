package link

// Wake frames require both CapWake and CapEvents. Production peers advertise
// CapWake once the spoke can durably claim, execute and reconcile a wake.
const (
	CapWake          = "wake"
	TypeWakeReady    = "wake_ready"
	TypeWakeReplay   = "wake_replay"
	TypeWakeResult   = "wake_result"
	TypeWakeRecorded = "wake_recorded"
	EventWake        = "wake"
	EventWakeClosed  = "wake_closed"
)

// WakeRoute is a spoke's current route evidence. Lane is an opaque token for
// the physical vendor queue, stable across route changes that keep that queue.
// Ready is asynchronous. Work appears as a sequenced event or a recovery
// frame, and there is no reply when the route has no eligible wake.
type WakeRoute struct {
	Member
	Scope       MemberScope `json:"scope"`
	Boot        string      `json:"boot"`
	Launch      string      `json:"launch"`
	PolicyEpoch int64       `json:"policy_epoch"`
	Lane        string      `json:"lane"`
}

// Wake carries the original authorization. An event and a recovery frame
// for the same attempt can only lead to one durable execution claim.
type Wake struct {
	AttemptID int64     `json:"attempt_id"`
	Route     WakeRoute `json:"route"`
	IDs       []int64   `json:"ids"`
}

const (
	WakeNotSubmitted = "not_submitted"
	WakeSubmitted    = "submitted"
	WakeUncertain    = "uncertain"
)

// WakeResult uses the original scope, including on a new spoke boot.
// SubmissionSeq is the original leaf cut from a durable execution claim.
// Submitted and uncertain results both require that cut.
// NotSubmitted requires durable proof that the spoke cannot later execute
// this attempt and that the transport made no submission.
type WakeResult struct {
	AttemptID     int64     `json:"attempt_id"`
	Route         WakeRoute `json:"route"`
	Outcome       string    `json:"outcome"`
	SubmissionSeq uint64    `json:"submission_seq,omitempty"`
}

// WakeClosed occupies the original event sequence after that wake stops
// giving execution authority. A waiting claim can still report its result.
type WakeClosed struct {
	AttemptID int64 `json:"attempt_id"`
}

type WakeRecorded struct {
	AttemptID int64  `json:"attempt_id"`
	Status    string `json:"status"`
	Code      string `json:"code,omitempty"`
}
