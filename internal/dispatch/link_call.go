package dispatch

import (
	"context"
	"encoding/json"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/link"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// linkOpVerbs need an operation key when a linked device forwards them,
// as they do from a local member.
var linkOpVerbs = map[string]bool{
	domain.VerbJoin: true, domain.VerbRead: true, domain.VerbAsk: true, domain.VerbReply: true,
	domain.VerbNote: true, domain.VerbLeave: true, domain.VerbFocus: true,
}

// linkResultLimit bounds a forwarded command's encoded result, so it fits
// one link frame with room for the frame's own fields.
const linkResultLimit = link.MaxFrame - 256

// linkCall runs a member command forwarded by a linked device (#400),
// inside the owner request's writer transaction. The order is fixed:
// the link's credential and the device's leaf registration, then an
// exact committed receipt for the member, then membership admission, then
// the command. Nothing here reads a leaf file or probes a process: the
// device authenticated its session against its own leaf.
//
// A newer leaf registration is part of authentication: it is kept
// whatever the command's outcome. The command itself runs inside a
// savepoint, so a refusal, or a result too large for one frame, undoes
// the command only. commit is false when the whole transaction must roll
// back, which happens only before authentication wrote anything.
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
		replay, found, err := domain.ReplayLinkReceipt(ctx, tx, *caller.PrincipalID, call)
		if err != nil || found {
			if err == nil {
				replay = fitLinkResult(replay)
			}
			return replay, err == nil, err
		}
	}
	// Admission for every caller: an unknown key joins only with an empty
	// selection.
	if !matchesMembership(call.Membership, caller.Membership) {
		return membershipChanged(wire.Request{Membership: call.Membership}), true, nil
	}
	if linkOpVerbs[call.Command] && call.OpKey == "" {
		return commandError(core.ExitUsage, "", "an operation key is required for this command"), true, nil
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT link_command`); err != nil {
		return core.Result{}, false, err
	}
	undo := func(r core.Result) (core.Result, bool, error) {
		if _, err := tx.ExecContext(ctx, `ROLLBACK TO link_command`); err != nil {
			return core.Result{}, false, err
		}
		_, err := tx.ExecContext(ctx, `RELEASE link_command`)
		return r, err == nil, err
	}
	ack, undoAck, err := domain.AcknowledgeRoster(ctx, tx, caller.BindingID, call.Ack, 0)
	if err != nil {
		return core.Result{}, false, err
	}
	request := domain.Request{OpKey: call.OpKey, Command: cmd, Vendor: call.Vendor, Enroll: member.Enroll, Ack: ack}
	if _, reading := cmd.(domain.Read); reading && call.WakePickup != nil {
		request.LinkPickup = &call
	}
	result, err = s.cfg.Handle.Handle(ctx, tx, caller, request)
	if err != nil {
		return core.Result{}, false, err
	}
	if result.Status != core.ExitOK {
		if caller.Role == core.RoleUnenrolled {
			return undo(result)
		}
		if undoAck != nil {
			if err := undoAck(); err != nil {
				return core.Result{}, false, err
			}
		}
	} else {
		principal, err := domain.LinkMemberPrincipal(ctx, tx, call)
		if err != nil {
			return core.Result{}, false, err
		}
		if principal != 0 {
			// The device stamps the member's current selection on its leaf.
			selected, err := domain.SelectedMembership(ctx, tx, principal)
			if err != nil {
				return core.Result{}, false, err
			}
			// As native ingress: a join that changes a selected member's
			// channel needs the selection it was issued under.
			if call.Command == domain.VerbJoin && call.Membership == nil && caller.Membership != nil &&
				caller.Membership.BindingID != 0 && selected.BindingID != caller.Membership.BindingID {
				return undo(domain.Fail(core.ExitRefused, core.CodeBindingChanged, "moving channels requires a current membership selection",
					"update crosshall on the linked device or retry the command once the link is ready"))
			}
			if err := domain.RecordLinkRequest(ctx, tx, principal, call); err != nil {
				return core.Result{}, false, err
			}
			if selected.BindingID != 0 {
				result.Membership = &selected
			}
		}
	}
	// A result the link cannot carry must not commit: a read would mark
	// messages picked up that the member never sees.
	if fitted := fitLinkResult(result); fitted.Code == domain.CodeLinkResultTooLarge {
		return undo(fitted)
	}
	if _, err := tx.ExecContext(ctx, `RELEASE link_command`); err != nil {
		return core.Result{}, false, err
	}
	if err := s.applyAdapterStamps(ctx, tx); err != nil {
		return core.Result{}, false, err
	}
	return result, true, nil
}

// fitLinkResult returns the result, or a refusal when its encoding would
// not fit one link frame.
func fitLinkResult(r core.Result) core.Result {
	raw, err := json.Marshal(r)
	if err == nil && len(raw) <= linkResultLimit {
		return r
	}
	return domain.Refuse(domain.CodeLinkResultTooLarge, "the answer is too large for the link",
		"read fewer messages at once, for example crosshall read <id>")
}

// linkReceipt answers a linked device's lookup of a forwarded request
// whose answer it lost: the committed receipt, or a fence that keeps the
// request from ever running (#400). It never runs the command. A refusal
// before the lookup commits nothing.
func (s *server) linkReceipt(ctx context.Context, tx store.Tx, req wire.Request) (core.Result, bool, error) {
	decoded, err := domain.Decode(req.Verb, req.Args)
	if err != nil {
		return commandError(core.ExitUsage, "", "invalid link receipt lookup"), false, nil
	}
	lookup := decoded.(domain.LinkReceipt)
	result, err := domain.FindOrFenceLinkReceipt(ctx, tx, lookup.LinkCall, time.Now())
	if err != nil {
		return core.Result{}, false, err
	}
	if result.Code == domain.CodeLinkStale || result.Status == core.ExitUsage {
		return result, false, nil
	}
	return fitLinkResult(result), true, nil
}
