package domain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	WhyNoRoute   = "no_route"  // no notice route, or none for its adapter
	WhyUnproven  = "unproven"  // notices not proven for its build
	WhySuspended = "suspended" // notices suspended, until UntilMS when timed
	WhyOff       = "off"       // switched off, do not disturb included
	WhyCapped    = "capped"    // the hourly limit, until UntilMS
	WhyHeld      = "held"      // an earlier notice is unobserved
	WhyKind      = "kind"      // this message starts no turn
)

// Where a member that is not woken sees the work.
const (
	SeenAtPrompt  = "prompt"  // its adapter offers pending work at a prompt
	SeenAtCommand = "command" // its next crosshall command
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
	var (
		offered, endsWait bool
		kind              string
	)
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM publish_attempt_item i
		JOIN publish_attempt a ON a.id = i.attempt_id
		WHERE i.message_id = m.id AND i.principal_id = ? AND a.state <> 'not_submitted'), m.kind, `+notMoreTime+`
		FROM message m WHERE m.id = ?`, principal, msg).Scan(&offered, &kind, &endsWait); err != nil {
		return Wake{}, err
	}
	if offered {
		return Wake{Wake: WakeNotice}, nil
	}
	w, err := memberWake(ctx, tx, binding, now)
	if err != nil {
		return Wake{}, err
	}
	if w.Wake == WakeNotice {
		// An available route offers only an open request or a final reply.
		var queued bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 `+activePublishRoutes(now)+
			` AND r.binding_id = ? AND EXISTS (`+publishMessages+` AND x.message_id = ?))`, binding, msg).Scan(&queued); err != nil {
			return Wake{}, err
		}
		if queued {
			return w, nil
		}
		w = Wake{Wake: WakeNone, Why: WhyKind}
	}
	// A request for more time ends no wait.
	if endsWait && s.waits.running(binding, kind, now) {
		return Wake{Wake: WakeWait}, nil
	}
	w.SeenAt, err = s.seenAt(ctx, tx, binding)
	return w, err
}

// promptAdapter joins, as ap, the adapter whose hooks offer a binding's
// pending work at a prompt, as owner who reports it: the current launch's
// adapter, enabled, the launch not fenced, the member not in do not disturb,
// and the binding its session's first.
const promptAdapter = `LEFT JOIN ` + currentBindingLaunch + `
	LEFT JOIN adapter_policy ap ON ap.adapter = json_extract(l.profile, '$.adapter') AND ap.enabled = 1
		AND l.fenced = 0 AND b.dnd = 0 AND (b.owner_hook_fence_ms IS NULL OR l.started_hook_at_ms > b.owner_hook_fence_ms)
		AND b.id = (SELECT MIN(id) FROM binding WHERE principal_id = b.principal_id)`

// seenAt says where a member that is not woken sees new work.
func (s *Service) seenAt(ctx context.Context, tx store.Tx, binding int64) (string, error) {
	var adapter, rev sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT ap.adapter, json_extract(l.profile, '$.manifest_rev')
		FROM binding b `+promptAdapter+` WHERE b.id = ?`, binding).Scan(&adapter, &rev)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if adapter.Valid && rev.Valid && s.HasPromptLine != nil && s.HasPromptLine(adapter.String, rev.String) {
		return SeenAtPrompt, nil
	}
	return SeenAtCommand, nil
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
