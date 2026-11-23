// Package domain defines the command handler boundary.
package domain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// Wire verbs for the commands in this package.
const (
	VerbCreateChannel = "owner channel create"
	VerbJoin          = "join"
	VerbWho           = "who"
)

// Request carries typed command arguments and the operation key.
type Request struct {
	OpKey   string
	Command Command

	// Vendor is the label of the caller's key (claude, codex, agy, route),
	// set by the dispatcher. It names generated aliases and roster rows.
	Vendor     string
	SessionKey string
	// Enroll links the caller's authenticated key to a principal created by
	// a first join, inside the same transaction. The dispatcher sets it when
	// the caller is unenrolled.
	Enroll func(ctx context.Context, tx store.Tx, principalID int64) error
	// Ack is the roster cursor this request acknowledges. The handler treats
	// it as already seen even if the dispatcher records it after the call.
	Ack uint64
	// Adapter contains dispatcher-verified context for a hook request.
	Adapter *AdapterContext
	// Host is the agent process the client named for a member command.
	Host *wire.Host
}

type AdapterContext struct {
	Instance, Generation   string
	NoticeSeq, ReminderSeq uint64
}

// Command is a decoded domain command.
type Command interface {
	command()
	// Verb returns the wire verb of the command.
	Verb() string
}

// CreateChannel creates a new channel.
type CreateChannel struct {
	Name  string `json:"name"`
	Topic string `json:"topic,omitempty"`
	// Pool is the number of root tokens the channel hands out, one at each
	// principal's first join; nil means DefaultJoinPool.
	Pool *int64 `json:"pool,omitempty"`
}

// DefaultJoinPool is the pool of a channel created without --pool. With it,
// requests nobody approved end after at most 8 exchanges of 8 units.
const DefaultJoinPool = 8

func (CreateChannel) command() {}

// Verb returns the wire verb.
func (CreateChannel) Verb() string { return VerbCreateChannel }

// Join binds the caller to a channel.
type Join struct {
	Channel  string `json:"channel,omitempty"`
	Alias    string `json:"alias,omitempty"`
	Focus    string `json:"focus,omitempty"`
	RepoRoot string `json:"repo_root,omitempty"`
	// Rules asks a repeated join to print the member rules again.
	Rules bool `json:"rules,omitempty"`
	// Launch is the agent process a hand join runs in, when the session's
	// adapter names one; the dispatcher binds it only if that process runs.
	Launch *JoinLaunch `json:"launch,omitempty"`
}

// The proposed launch is new on every call; a retried join with the same key
// is the same join, and replays the launch the first call bound.
func (j Join) normalized() Command {
	j.Launch = nil
	return j
}

// JoinLaunch is a launch reference a join proposes, as a start would bind it.
type JoinLaunch struct {
	Ref          string           `json:"ref"`
	AtMS         int64            `json:"at_ms"`
	Profile      wire.HookProfile `json:"profile"`
	PIDNamespace string           `json:"pid_namespace,omitempty"`
}

func (Join) command() {}

// Verb returns the wire verb.
func (Join) Verb() string { return VerbJoin }

// Who lists members in the caller's channel.
type Who struct {
	ExitTracking bool `json:"exit_tracking,omitempty"`
}

func (Who) command() {}

// Verb returns the wire verb.
func (Who) Verb() string { return VerbWho }

// Handler executes a command within the dispatcher-owned transaction.
type Handler interface {
	Handle(ctx context.Context, tx store.Tx, caller core.Caller, request Request) (core.Result, error)
}

// Decode turns a wire verb and its JSON arguments into a typed command.
// Unknown fields are rejected so that a newer client cannot be misread.
func Decode(verb string, args json.RawMessage) (Command, error) {
	var cmd Command
	switch verb {
	// adapter-domain
	case VerbHookEnroll:
		return decodeLedgerCommand[HookEnroll](args)
	case VerbHookEvent:
		return decodeLedgerCommand[HookEvent](args)
	case VerbHookRoute:
		return decodeLedgerCommand[HookRoute](args)
	// end adapter-domain
	// owner-views
	case VerbOwnerWho:
		return decodeLedgerCommand[OwnerWho](args)
	case VerbOwnerDepart:
		return decodeLedgerCommand[OwnerDepart](args)
	case VerbOwnerLog:
		return decodeLedgerCommand[OwnerLog](args)
	case VerbChannelDefault:
		return decodeLedgerCommand[ChannelDefault](args)
	case VerbChannelAuto:
		return decodeLedgerCommand[ChannelAuto](args)
	// end owner-views
	case VerbAdapterSet:
		return decodeLedgerCommand[AdapterSet](args)
	case VerbPublish:
		return decodeLedgerCommand[Publish](args)
	case VerbOwnerPublish:
		return decodeLedgerCommand[OwnerPublish](args)
	case VerbAdapterEvidence: // adapter-claude
		return decodeLedgerCommand[AdapterEvidence](args)
	// read-ledger
	case VerbRead:
		return decodeLedgerCommand[Read](args)
	case VerbInbox:
		return decodeLedgerCommand[Inbox](args)
	case VerbWait:
		return decodeLedgerCommand[Wait](args)
	case VerbWaiting:
		return decodeLedgerCommand[Waiting](args)
	case VerbStatus:
		return decodeLedgerCommand[Status](args)
	case VerbOwnerStatus:
		return decodeLedgerCommand[OwnerStatus](args)
	case VerbCancel:
		return decodeLedgerCommand[Cancel](args)
	case VerbBudgetGrant:
		return decodeLedgerCommand[BudgetGrant](args)
	case VerbBudgetPolicy:
		return decodeLedgerCommand[BudgetPolicy](args)
	// end read-ledger
	case VerbCreateChannel:
		var c CreateChannel
		if err := strictUnmarshal(args, &c); err != nil {
			return nil, err
		}
		cmd = c
	case VerbRestore:
		var c Restore
		if err := strictUnmarshal(args, &c); err != nil {
			return nil, err
		}
		cmd = c
	case VerbJoin:
		var c Join
		if err := strictUnmarshal(args, &c); err != nil {
			return nil, err
		}
		cmd = c
	case VerbWho:
		var c Who
		if err := strictUnmarshal(args, &c); err != nil {
			return nil, err
		}
		cmd = c
	// ask-reply
	case VerbAsk:
		var c Ask
		if err := strictUnmarshal(args, &c); err != nil {
			return nil, err
		}
		cmd = c
	case VerbReply:
		var c Reply
		if err := strictUnmarshal(args, &c); err != nil {
			return nil, err
		}
		cmd = c
	// end ask-reply
	// note-log
	case VerbNote:
		return decodeLedgerCommand[Note](args)
	case VerbLog:
		return decodeLedgerCommand[Log](args)
	case VerbLeave:
		return decodeLedgerCommand[Leave](args)
	case VerbFocus:
		return decodeLedgerCommand[Focus](args)
	// end note-log
	default:
		return nil, fmt.Errorf("unknown verb %q", verb)
	}
	return cmd, nil
}

func strictUnmarshal(args json.RawMessage, v any) error {
	if len(bytes.TrimSpace(args)) == 0 {
		args = json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("bad arguments: %w", err)
	}
	return nil
}
