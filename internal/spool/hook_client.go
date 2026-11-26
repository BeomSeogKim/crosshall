package spool

import (
	"context"
	"errors"
	"os"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// OpenHookLeaf pins the private member directory and reads its current generation.
func OpenHookLeaf(rv *os.Root, id ident.Identity) (*os.Root, string, error) {
	return openLeaf(rv, id.Digest())
}

// hookRoot opens the member rendezvous, a linked device's when its
// dispatcher names one (#400); linked says which.
func (c Client) hookRoot(ctx context.Context, id ident.Identity) (*os.Root, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if _, ok := ctx.Deadline(); !ok {
		return nil, false, errors.New("hook deadline is required")
	}
	if id.Vendor == "" || id.Vendor == "route" || id.Key == "" {
		return nil, false, errors.New("hook requires a vendor session identity")
	}
	members := memberRendezvous(c.Rendezvous)
	rv, err := ident.OpenPrivateDir(members)
	return rv, members != c.Rendezvous, err
}

// HookLookup performs exactly one first contact. Only a dispatcher refusal
// carrying not_enrolled authorizes a subsequent repository default check.
func (c Client) HookLookup(ctx context.Context, id ident.Identity, intent string) (string, wire.Response, error) {
	if intent != "lookup" && intent != "join" && intent != wire.HelloStart {
		return "", wire.Response{}, errors.New("invalid hook hello intent")
	}
	rv, _, err := c.hookRoot(ctx, id)
	if err != nil {
		return "", wire.Response{}, err
	}
	defer rv.Close()
	resp, err := firstContact(ctx, rv, id, intent)
	if err != nil || resp.Status != core.ExitOK {
		return "", resp, err
	}
	leaf, generation, err := OpenHookLeaf(rv, id)
	if err != nil {
		return "", resp, err
	}
	leaf.Close()
	return generation, resp, nil
}

// HookCall targets the generation used to evaluate this callback. Hooks
// never refresh a lease or participate in the ordinary roster cursor.
func (c Client) HookCall(ctx context.Context, id ident.Identity, generation string, req wire.Request, oneWay bool) (Reply, error) {
	switch req.Verb {
	case wire.VerbHookEvent:
	case wire.VerbHookEnroll:
		if oneWay {
			return Reply{}, errors.New("unsupported hook operation")
		}
	case wire.VerbHookRoute, wire.VerbHookJoinHintAck:
		if !oneWay {
			return Reply{}, errors.New("unsupported hook operation")
		}
	default:
		return Reply{}, errors.New("unsupported hook operation")
	}
	if generation == "" || req.OpKey == "" {
		return Reply{}, errors.New("hook requires generation and operation key")
	}
	rv, linked, err := c.hookRoot(ctx, id)
	if err != nil {
		return Reply{}, err
	}
	defer rv.Close()
	leaf, current, err := OpenHookLeaf(rv, id)
	if err != nil {
		return Reply{}, err
	}
	defer leaf.Close()
	if generation != current {
		return Reply{}, errors.New("member generation changed before hook publication")
	}
	if req.Membership != nil {
		current, err := ident.LookupMembership(leaf)
		if err != nil {
			return Reply{}, err
		}
		if current == nil || *current != *req.Membership {
			return Reply{}, errors.New("membership changed before hook publication")
		}
	}
	req = prepare(ctx, req)
	req.KeyDigest, req.Generation, req.VendorVersion = id.Digest(), generation, id.Version
	req.Capture = nil
	if linked {
		req.Capture = &wire.HookCapture{} // filled under the publish lock
	}
	req.Ack = 0
	req.Reply = nil
	if oneWay {
		in, err := ident.OpenPrivateChild(leaf, "in")
		if err != nil {
			return Reply{}, err
		}
		defer in.Close()
		wantsReply := false
		req.Reply = &wantsReply
		err = publishHookRequest(ctx, leaf, in, req)
		return Reply{OpKey: req.OpKey, RequestMembership: req.Membership}, err
	}
	resp, _, err := exchangeWithPublisher(ctx, leaf, req.Attempt, func(in *os.Root) error {
		return publishHookRequest(ctx, leaf, in, req)
	})
	return Reply{Response: resp, OpKey: req.OpKey, RequestMembership: req.Membership}, err
}
