// Package core defines the shared caller and command result types.
package core

import (
	"encoding/json"
	"time"
)

// Role is assigned by the dispatcher after checking the ingress.
type Role string

const (
	RoleOwner      Role = "owner"
	RoleMember     Role = "member"
	RoleUnenrolled Role = "unenrolled"
)

// Caller is the resolved identity passed to a command handler.
type Caller struct {
	Role        Role
	PrincipalID *int64
	BindingID   *int64
}

// Clock supplies the current time to a command handler.
type Clock func() time.Time

// ExitCode is the command's process exit status.
type ExitCode int

const (
	ExitOK          ExitCode = 0
	ExitError       ExitCode = 1
	ExitUsage       ExitCode = 2
	ExitRefused     ExitCode = 3
	ExitNotEnrolled ExitCode = 4
	ExitUnavailable ExitCode = 5
	ExitWaitTimeout ExitCode = 6
)

// Code identifies a refusal or transport outcome.
type Code string

const (
	CodeUnknownAlias       Code = "unknown_alias"
	CodeDeparted           Code = "departed"
	CodeNotFound           Code = "not_found"
	CodeArchived           Code = "archived"
	CodeEachRequired       Code = "each_required"
	CodeInsufficientCredit Code = "insufficient_credit"
	CodeBudgetExhausted    Code = "budget_exhausted"
	CodeNoRootCredit       Code = "no_root_credit"
	CodeMustAnswerFirst    Code = "must_answer_first"
	CodeAmbiguousRoot      Code = "ambiguous_root"
	CodeBadRoot            Code = "bad_root"
	CodeNotObligated       Code = "not_obligated"
	CodeAlreadySettled     Code = "already_settled"
	CodeCancelled          Code = "cancelled"
	CodeKeyConflict        Code = "key_conflict"
	CodeKeyInUse           Code = "key_in_use"
	CodeAliasTaken         Code = "alias_taken"
	CodeOneChannel         Code = "one_channel"
	CodeOwnerOnly          Code = "owner_only"
	CodeChannelExists      Code = "channel_exists"

	CodeNotEnrolled      Code = "not_enrolled"
	CodeBindingAmbiguous Code = "binding_ambiguous"
	CodeKeyRetired       Code = "key_retired"
	CodeLeafMismatch     Code = "leaf_mismatch"

	CodeUnsent         Code = "unsent"
	CodeOutcomeUnknown Code = "outcome_unknown"
	CodeWaitTimeout    Code = "wait_timeout"
)

// Result is the command outcome shared by the handler, dispatcher and CLI.
// Data is the command-specific JSON, which the CLI renders for either output mode.
type Result struct {
	Status ExitCode        `json:"status"`
	Code   Code            `json:"code"`
	Data   json.RawMessage `json:"data"`
	// JoinHintAcked is current delivery metadata, populated after operation replay.
	JoinHintAcked bool `json:"join_hint_acked,omitempty"`
}
