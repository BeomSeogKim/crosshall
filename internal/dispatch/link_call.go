package dispatch

import (
	"context"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// linkOpVerbs need an operation key when a linked device forwards them,
// as they do from a local member.
var linkOpVerbs = map[string]bool{
	domain.VerbJoin: true, domain.VerbRead: true, domain.VerbAsk: true, domain.VerbReply: true,
	domain.VerbNote: true, domain.VerbLeave: true, domain.VerbFocus: true,
}

// linkCall runs a member command forwarded by a linked device (#400),
// inside the owner request's writer transaction. The order is fixed:
// the link's credential and the device's leaf registration, then an
// exact committed receipt for the member, then membership admission, then
// the command. Nothing here reads a leaf file or probes a process: the
// device authenticated its session against its own leaf.
// commit is false when the transaction must roll back.
func (s *server) linkCall(ctx context.Context, tx store.Tx, req wire.Request) (result core.Result, commit bool, err error) {
	decoded, err := domain.Decode(req.Verb, req.Args)
	if err != nil {
		return commandError(core.ExitUsage, "", "invalid link call"), false, nil
	}
	call := decoded.(domain.LinkCall)
	cmd, refusal := domain.LinkCommand(call.Command, call.Args)
	if refusal != nil {
		return *refusal, false, nil
	}
	now := time.Now()
	member, refusal, err := domain.ResolveLinkMember(ctx, tx, call, now)
	if err != nil || refusal != nil {
		if refusal != nil {
			return *refusal, false, nil
		}
		return core.Result{}, false, err
	}
	caller := member.Caller
	if caller.PrincipalID != nil {
		replay, found, err := domain.ReplayLinkReceipt(ctx, tx, *caller.PrincipalID, call.OpKey, cmd, call.Membership)
		if err != nil || found {
			return replay, false, err
		}
		if !matchesMembership(call.Membership, caller.Membership) {
			return membershipChanged(wire.Request{Membership: call.Membership}), false, nil
		}
	}
	if linkOpVerbs[call.Command] && call.OpKey == "" {
		return commandError(core.ExitUsage, "", "an operation key is required for this command"), false, nil
	}
	ack, undoAck, err := domain.AcknowledgeRoster(ctx, tx, caller.BindingID, call.Ack, 0)
	if err != nil {
		return core.Result{}, false, err
	}
	request := domain.Request{OpKey: call.OpKey, Command: cmd, Vendor: call.Vendor, Enroll: member.Enroll, Ack: ack}
	result, err = s.cfg.Handle.Handle(ctx, tx, caller, request)
	if err != nil {
		return core.Result{}, false, err
	}
	if result.Status != core.ExitOK {
		if caller.Role == core.RoleUnenrolled {
			return result, false, nil
		}
		if undoAck != nil {
			return result, true, undoAck()
		}
		return result, true, nil
	}
	principal, err := domain.LinkMemberPrincipal(ctx, tx, call)
	if err != nil {
		return core.Result{}, false, err
	}
	if principal != 0 {
		if err := domain.RecordLinkMembership(ctx, tx, principal, call.OpKey, call.Membership); err != nil {
			return core.Result{}, false, err
		}
		// The device stamps the member's current selection on its leaf.
		selected, err := domain.SelectedMembership(ctx, tx, principal)
		if err != nil {
			return core.Result{}, false, err
		}
		if selected.BindingID != 0 {
			result.Membership = &selected
		}
	}
	if err := s.applyAdapterStamps(ctx, tx); err != nil {
		return core.Result{}, false, err
	}
	return result, true, nil
}
