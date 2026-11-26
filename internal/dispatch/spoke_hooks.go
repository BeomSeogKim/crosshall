package dispatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/hook"
	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
	"github.com/BeomSeogKim/crosshall/internal/hostclock"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/link"
	"github.com/BeomSeogKim/crosshall/internal/notice"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// spokePolicyFiles publishes the hook policy files of a linked device from
// the policies its cache holds, in the native format, or withdraws them
// so its hooks go quiet (#400, 12k).
func (s *server) spokePolicyFiles(ctx context.Context, publish bool) error {
	root, err := spool.OpenServerRoot(s.cfg.Rendezvous)
	if err != nil {
		return err
	}
	defer root.Close()
	if !publish {
		for _, remove := range []func() error{
			func() error { return root.Remove(adapterPolicyName) },
			func() error { return s.state.Remove(hook.StatePolicyName) },
		} {
			if err := remove(); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		return nil
	}
	var text string
	err = s.spokeMembers.db.View(ctx, func(tx store.Tx) error {
		var err error
		text, err = spokePolicyText(ctx, tx)
		return err
	})
	if err != nil {
		return err
	}
	return errors.Join(publishAdapterText(root, adapterPolicyName, text), publishAdapterText(s.state, hook.StatePolicyName, text))
}

// spokePolicyText is adapterPolicyText over the cached policies.
func spokePolicyText(ctx context.Context, tx store.Tx) (string, error) {
	var out strings.Builder
	for _, adapter := range manifests.Names() {
		var enabled, reminder int
		var epoch uint64
		err := tx.QueryRowContext(ctx, `SELECT enabled, reminder, epoch FROM spoke_policy WHERE adapter = ?`, adapter).
			Scan(&enabled, &reminder, &epoch)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", err
		}
		fmt.Fprintf(&out, "%s %d %d %d\n", adapter, enabled, reminder, epoch)
	}
	return out.String(), nil
}

// spokeHookFacts are the hook facts the home records from a linked device.
var spokeHookFacts = map[string]bool{
	wire.HookPromptSubmitted: true, wire.HookTurnFinishing: true, wire.HookApprovalRequested: true,
	wire.HookSessionEnded: true, wire.HookIdlePrompt: true,
}

// spokeHook serves a hook of a linked device's session (#400, 12k): its
// fact, or the written acknowledgement of an offer it printed, goes to the
// hook outbox with the token the hook took at capture, in a committed
// transaction; its output comes only from an eligible cache.
func (s *server) spokeHook(ctx context.Context, path string, dir *os.Root, req wire.Request) (core.Result, error) {
	if req.Verb != wire.VerbHookEnroll && req.Verb != wire.VerbHookEvent && req.Verb != wire.VerbHookJoinHintAck {
		return domain.Fail(core.ExitUnavailable, core.CodeDispatcherUnavailable,
			"hook routes of a linked device reach the hall in a later release", ""), nil
	}
	// As natively, the acknowledgement of output already written is
	// recorded however late it arrives.
	if req.Verb != wire.VerbHookJoinHintAck && req.ExpiresAtMS <= time.Now().UnixMilli() {
		return unknownOutcome("attempt expired", req.OpKey), nil
	}
	capture := req.Capture
	if capture == nil || req.PublishSeq == 0 || capture.Boot == "" || capture.MonoNS < 0 {
		return commandError(core.ExitUsage, "", "a linked device's hook carries its capture token"), nil
	}
	notMember := commandError(core.ExitNotEnrolled, core.CodeNotEnrolled, "hook event requires a live member")
	var fact *link.Fact
	var written *link.Written
	var adapter, launch string
	var epoch uint64
	output := !req.OneWayHook()
	switch req.Verb {
	case wire.VerbHookEnroll:
		var args wire.HookEnrollArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return commandError(core.ExitUsage, "", "invalid hook enrollment"), nil
		}
		adapter, launch, epoch = args.Profile.Adapter, args.LaunchRef, args.Epoch
		args.PIDNamespace, args.Host, args.VendorHome = "", nil, ""
		args.RepoRoot, args.Repository, args.CreateChannel = "", "", false
		body, err := json.Marshal(args)
		if err != nil {
			return core.Result{}, err
		}
		fact = &link.Fact{Kind: req.Verb, Body: body}
	case wire.VerbHookEvent:
		var args wire.HookEventArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return commandError(core.ExitUsage, "", "invalid hook event"), nil
		}
		adapter, launch, epoch = args.Profile.Adapter, args.LaunchRef, args.Epoch
		args.PIDNamespace, args.Host, args.VendorHome = "", nil, ""
		args.EnrollRepoRoot, args.EnrollRepository, args.EnrollCreate = "", "", false
		if spokeHookFacts[args.Fact] {
			body, err := json.Marshal(args)
			if err != nil {
				return core.Result{}, err
			}
			fact = &link.Fact{Kind: req.Verb, Body: body}
		}
	case wire.VerbHookJoinHintAck:
		var args wire.HookOutputAckArgs
		if err := json.Unmarshal(req.Args, &args); err != nil {
			return commandError(core.ExitUsage, "", "invalid hook acknowledgement"), nil
		}
		if args.Offer != nil {
			at := args.WrittenAtMS
			if at <= 0 {
				at = time.Now().UnixMilli()
			}
			written = &link.Written{OfferID: args.Offer.OfferID, Nonce: args.Offer.Nonce, WrittenAtMS: at}
		}
	}
	m := s.spokeMembers
	var result wire.HookResult
	var refusal *core.Result
	queued := false
	err := m.db.Update(ctx, func(tx store.Tx) error {
		leaf, err := ident.ResolveSpokeMember(ctx, tx, m.members, ident.Ingress{Path: path, Dir: dir},
			ident.Proof{KeyDigest: req.KeyDigest, Generation: req.Generation})
		if err != nil {
			return err
		}
		if capture.Membership == nil || capture.Membership.BindingID <= 0 || capture.Membership.Generation <= 0 {
			refusal = &notMember
			return nil
		}
		member := link.Member{Vendor: leaf.Vendor, Key: leaf.Key}
		token := link.HookToken{Boot: capture.Boot, LeafSeq: req.PublishSeq, MonoNS: capture.MonoNS, WallMS: capture.WallMS,
			LeafGeneration: leaf.Generation, Membership: link.Membership(*capture.Membership)}
		if written != nil {
			written.Member, written.Token = member, token
			if _, err := tx.ExecContext(ctx, `UPDATE spoke_offer SET suppressed = 1 WHERE vendor = ? AND key = ? AND offer_id = ?
				AND json_extract(body, '$.nonce') = ?`, leaf.Vendor, leaf.Key, written.OfferID, written.Nonce); err != nil {
				return err
			}
			queued = true
			return queueHookFrame(ctx, tx, "written", member, req.PublishSeq, written)
		}
		if adapter != leaf.Vendor || req.OpKey != "hook:"+adapter+":"+launch+":"+hookEventRef(req) {
			r := commandError(core.ExitRefused, "", "hook identity does not match the member exchange")
			refusal = &r
			return nil
		}
		var enabled bool
		var current uint64
		err = tx.QueryRowContext(ctx, `SELECT enabled, epoch FROM spoke_policy WHERE adapter = ?`, adapter).Scan(&enabled, &current)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if !enabled || current != epoch {
			r := commandError(core.ExitRefused, "", "adapter policy is off or has changed")
			refusal = &r
			return nil
		}
		result.Epoch = epoch
		if fact != nil {
			fact.Member, fact.Token = member, token
			queued = true
			if err := queueHookFrame(ctx, tx, "fact", member, req.PublishSeq, fact); err != nil {
				return err
			}
		}
		if output && req.OfferReceipts {
			return s.spokeOffer(ctx, tx, leaf, *capture.Membership, launch, epoch, &result)
		}
		return nil
	})
	var env *ident.EnvironmentError
	if errors.As(err, &env) {
		return domain.Fail(core.ExitNotEnrolled, env.Code, env.Message, "crosshall join <channel>"), nil
	}
	if err != nil {
		return core.Result{}, err
	}
	if refusal != nil {
		return *refusal, nil
	}
	if queued {
		s.spokeMembers.hooksQueued.Store(true) // sent after this drain pass
	}
	return domain.OK(result)
}

// hookEventRef is the event reference of a hook request's arguments.
func hookEventRef(req wire.Request) string {
	var args struct {
		EventRef string `json:"event_ref"`
	}
	_ = json.Unmarshal(req.Args, &args)
	return args.EventRef
}

func queueHookFrame(ctx context.Context, tx store.Tx, kind string, member link.Member, leafSeq uint64, frame any) error {
	body, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO spoke_hook_outbox (vendor, key, leaf_seq, kind, body, at_ms) VALUES (?, ?, ?, ?, ?, ?)`,
		member.Vendor, member.Key, leafSeq, kind, string(body), time.Now().UnixMilli())
	return err
}

// spokeOffer sets the pending line from the member's cached offer, when the
// cache is eligible and the offer is the one the home prepared for this
// leaf, membership, launch and policy epoch, unsuppressed and unexpired.
func (s *server) spokeOffer(ctx context.Context, tx store.Tx, leaf ident.SpokeLeaf, membership core.Membership,
	launch string, epoch uint64, out *wire.HookResult) error {
	var eligible bool
	var boot string
	err := tx.QueryRowContext(ctx, `SELECT eligible, boot FROM spoke_event_cursor WHERE id = 1`).Scan(&eligible, &boot)
	if errors.Is(err, sql.ErrNoRows) || err == nil && !eligible {
		return nil
	}
	if err != nil {
		return err
	}
	mono := s.spokeMembers.mono
	if mono == nil {
		mono = hostclock.Mono
	}
	now, err := mono()
	if err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT body FROM spoke_offer WHERE vendor = ? AND key = ? AND suppressed = 0
		AND boot = ? AND deadline_mono > ? ORDER BY offer_id DESC`, leaf.Vendor, leaf.Key, boot, now)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return err
		}
		var o link.Offer
		if json.Unmarshal([]byte(body), &o) != nil || len(o.IDs) == 0 || o.LeafGeneration != leaf.Generation ||
			o.Membership != link.Membership(membership) || o.Launch != launch || o.PolicyEpoch != int64(epoch) {
			continue
		}
		out.PendingLine = notice.PendingPrefix + notice.IDList(o.IDs)
		out.OfferID = o.OfferID
		out.OfferReceipt = &wire.HookOfferReceipt{OfferID: o.OfferID, Nonce: o.Nonce}
		out.Ack = &wire.HookAck{Kind: "notice"}
		return nil
	}
	return rows.Err()
}

// spokeHookQueued wakes the event client to send what the hook outbox holds.
func (s *server) spokeHookQueued() {
	select {
	case s.spokeMembers.hooks <- struct{}{}:
	default:
	}
}
