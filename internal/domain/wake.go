package domain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// Wake values: what tells a member about new work without a person typing.
const (
	WakeNotice = "notice" // the hall starts a turn in its session
	WakeWait   = "wait"   // a wait of the member is running
	WakeNone   = "none"   // nothing does
)

// Why words of WakeNone. They are fixed, and the same in a send result, in
// who and in waiting; binding IDs, owner commands and reason details stay in
// the owner's views.
const (
	WhyNoRoute   = "no_route"          // no notice route, or none for its adapter
	WhyUnproven  = "unproven"          // notices not proven for its build
	WhySuspended = "suspended"         // notices suspended, until UntilMS when timed
	WhyOff       = "off"               // switched off, do not disturb included
	WhyCapped    = "capped"            // the hourly limit, until UntilMS
	WhyHeld      = "held"              // an earlier notice is unobserved
	WhyTurnOpen  = "turn_open"         // the member already has a running turn
	WhyKind      = "kind"              // this message starts no turn
	WhyApproval  = "awaiting_approval" // its turn waits on its owner's approval
)

// Where a member that is not woken sees the work.
const (
	SeenAtTurnEnd = "turn_end" // the turn-end check of its running turn
	SeenAtPrompt  = "prompt"   // its adapter offers pending work at a prompt
	SeenAtCommand = "command"  // its next crosshall command
)

// Wake is what will tell a member about new work, as a sender sees it.
type Wake struct {
	Wake string `json:"wake,omitempty"`
	Why  string `json:"why,omitempty"`
	// Adapter names the adapter that has no sender, with no_route.
	Adapter string `json:"adapter,omitempty"`
	SeenAt  string `json:"seen_at,omitempty"`
	UntilMS int64  `json:"until_ms,omitempty"`
}

// NeedsManualWake reports that neither a current delivery path nor a deferred
// notice can tell this recipient about the message.
func (w Wake) NeedsManualWake() bool {
	return w.Wake == WakeNone && w.SeenAt != SeenAtTurnEnd && w.Why != WhyTurnOpen && w.Why != WhyApproval
}

// reachWake is what a member's publish state says about any new work.
func reachWake(status store.PublishState, found bool) Wake {
	none := func(why string) Wake { return Wake{Wake: WakeNone, Why: why} }
	if !found {
		return none(WhyNoRoute)
	}
	switch status.State {
	case "available":
		return Wake{Wake: WakeNotice}
	case "suspended":
		w := none(WhySuspended)
		w.UntilMS = status.UntilMS
		return w
	case "capped":
		w := none(WhyCapped)
		w.UntilMS = status.UntilMS
		return w
	case "held":
		return none(WhyHeld)
	case "off":
		if status.Invalidated {
			return none(WhyNoRoute)
		}
		return none(WhyOff)
	}
	if adapter, ok := strings.CutPrefix(status.Reason, store.PublishNoSender); ok {
		w := none(WhyNoRoute)
		w.Adapter = adapter
		return w
	}
	switch status.Reason {
	// A Codex session outside a confirmed managed app server has no route a
	// notice could take.
	case publish.CodexNoDaemon, publish.CodexDaemonUnverified:
		w := none(WhyNoRoute)
		w.Adapter = "codex"
		return w
	}
	return none(WhyUnproven)
}

// memberWake is what will tell a live binding about new work now.
func memberWake(ctx context.Context, tx store.Tx, binding int64, now time.Time) (Wake, error) {
	status, found, err := store.ReadPublishState(ctx, tx, binding, now)
	return reachWake(status, found), err
}

// messageWake is what will tell a live binding about stored message msg: a
// notice that offered it or will, else the member's running wait when it
// accepts the message, else nothing, with the reason and where the member
// sees it.
func (s *Service) messageWake(ctx context.Context, tx store.Tx, binding, principal, msg int64, now time.Time) (Wake, error) {
	w, submitted, err := s.messageWakeOf(ctx, tx, binding, principal, msg, now)
	if err != nil || submitted {
		return w, err
	}
	// A turn blocked on an approval moves only when its owner answers: no
	// notice, wait or second request can move it. Where the member sees the
	// message once it does still holds. Past the fact's cap nothing is
	// claimed: a denied dialog sends no Stop.
	fact, err := latestFact(ctx, tx, principal)
	if err != nil || fact == nil || !fact.ApprovalOpen || !fact.At(ms(now)).Current {
		return w, err
	}
	return Wake{Wake: WakeNone, Why: WhyApproval, SeenAt: w.SeenAt}, nil
}

// messageWakeOf is messageWake before an open approval; submitted says a
// notice already passed the submission fence.
func (s *Service) messageWakeOf(ctx context.Context, tx store.Tx, binding, principal, msg int64, now time.Time) (Wake, bool, error) {
	var (
		attempt, kind string
		endsWait      bool
		channel       int64
	)
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE((SELECT a.state FROM publish_attempt_item i
		JOIN publish_attempt a ON a.id = i.attempt_id
		WHERE i.message_id = x.message_id AND i.principal_id = x.principal_id AND a.state <> 'not_submitted'
			AND (a.state <> 'reserved' OR (`+pendingPublishItem+`
				AND EXISTS (SELECT 1 `+activePublishRoutes(now)+` AND `+reservationMatchesRoute+`)))
		ORDER BY a.id DESC LIMIT 1), ''), m.kind, `+notMoreTime+`, m.channel_id
		FROM message m JOIN recipient x ON x.message_id = m.id WHERE x.principal_id = ? AND m.id = ?`,
		principal, msg).Scan(&attempt, &kind, &endsWait, &channel); err != nil {
		return Wake{}, false, err
	}
	if attempt != "" && attempt != "reserved" {
		return Wake{Wake: WakeNotice}, true, nil // already passed the submission fence
	}
	eligible := attempt == "reserved"
	var w Wake
	if !eligible {
		var err error
		w, err = memberWake(ctx, tx, binding, now)
		if err != nil {
			return Wake{}, false, err
		}
		if w.Wake == WakeNotice {
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 `+activePublishRoutes(now)+
				` AND r.binding_id = ? AND EXISTS (`+publishMessages+` AND x.message_id = ?))`, binding, msg).Scan(&eligible); err != nil {
				return Wake{}, false, err
			}
			w = Wake{Wake: WakeNone, Why: WhyKind}
		}
	}
	var running bool
	var err error
	if eligible || kind == "ask" {
		running, err = inReportedTurn(ctx, tx, principal, now)
		if err != nil {
			return Wake{}, false, err
		}
	}
	if eligible {
		if !running {
			return Wake{Wake: WakeNotice}, false, nil
		}
		w = Wake{Wake: WakeNone, Why: WhyTurnOpen}
	}
	// A request for more time ends no wait.
	if endsWait {
		accepts, err := s.waits.accepts(ctx, tx, binding, kind, msg, now)
		if err != nil {
			return Wake{}, false, err
		}
		if accepts {
			return Wake{Wake: WakeWait}, false, nil
		}
	}
	if kind == "ask" && running {
		turnEnd, err := turnEndCovered(ctx, tx, principal)
		if err != nil {
			return Wake{}, false, err
		}
		// The turn end offers only the first OfferCap requests.
		if turnEnd {
			offer, err := turnEndOffer(ctx, tx, principal, channel)
			if err != nil || slices.Contains(offer, msg) {
				w.SeenAt = SeenAtTurnEnd
				return w, false, err
			}
		}
	}
	if w.Why == WhyTurnOpen {
		return w, false, nil // the publisher will check again when the turn ends
	}
	w.SeenAt, err = s.seenAt(ctx, tx, binding)
	return w, false, err
}

// turnEndCovered reports whether the current launch can run the turn-end check.
func turnEndCovered(ctx context.Context, tx store.Tx, principal int64) (bool, error) {
	var covered bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM adapter_launch l
		WHERE l.rowid = (SELECT latest.rowid FROM adapter_launch latest
				WHERE latest.principal_id = ?1 AND latest.ended_hook_at_ms IS NULL
				ORDER BY latest.started_hook_at_ms DESC, latest.rowid DESC LIMIT 1)
			AND `+store.CapabilityCovered("json_extract(l.profile,'$.adapter')", "l.profile", "'turn_end'")+`
			AND `+store.TurnEndSwitchOn("json_extract(l.profile,'$.adapter')")+`)`, principal).Scan(&covered)
	return covered, err
}

// promptAdapter joins, as ap, the adapter whose hooks offer a binding's
// pending work at a prompt, as owner who reports it: the current launch's
// adapter, enabled, the launch not fenced, the member not in do not disturb,
// and the binding selected for its session.
const promptAdapter = `LEFT JOIN ` + currentBindingLaunch + `
	LEFT JOIN adapter_policy ap ON ap.adapter = json_extract(l.profile, '$.adapter') AND ap.enabled = 1
		AND l.fenced = 0 AND b.dnd = 0 AND (b.owner_hook_fence_ms IS NULL OR l.started_hook_at_ms > b.owner_hook_fence_ms)
		AND b.id = (SELECT binding_id FROM member_selection WHERE principal_id = b.principal_id)`

// seenAt says where a member that is not woken sees new work.
func (s *Service) seenAt(ctx context.Context, tx store.Tx, binding int64) (string, error) {
	adapter, err := s.promptLineAdapter(ctx, tx, binding)
	if err != nil || adapter == "" {
		return SeenAtCommand, err
	}
	return SeenAtPrompt, nil
}

// promptLineAdapter names the adapter whose prompt line shows the member
// new work at its next prompt, or "" when its next prompt does not.
func (s *Service) promptLineAdapter(ctx context.Context, tx store.Tx, binding int64) (string, error) {
	var adapter, rev sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT ap.adapter, json_extract(l.profile, '$.manifest_rev')
		FROM binding b `+promptAdapter+` WHERE b.id = ?`, binding).Scan(&adapter, &rev)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if adapter.Valid && rev.Valid && s.HasPromptLine != nil && s.HasPromptLine(adapter.String, rev.String) {
		return adapter.String, nil
	}
	return "", nil
}

// sendWakeEvent records, for the daily report, what will tell one recipient
// of a request or a final reply when it is sent.
const sendWakeEvent = "send_wake"

func recordWakes(ctx context.Context, tx store.Tx, sender, msg int64, recipients []Delivery, now time.Time) error {
	for _, d := range recipients {
		if d.Wake.Wake == "" {
			continue // held for a recipient that left
		}
		detail := struct {
			Wake string `json:"wake"`
			Why  string `json:"why,omitempty"`
		}{d.Wake.Wake, d.Why}
		if err := audit(ctx, tx, sendWakeEvent, fmt.Sprintf("message:%d", msg), sender, detail, now); err != nil {
			return err
		}
	}
	return nil
}
