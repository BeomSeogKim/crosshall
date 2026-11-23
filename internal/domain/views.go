package domain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

const (
	VerbInbox       = "inbox"
	VerbWaiting     = "waiting"
	VerbStatus      = "status"
	VerbOwnerStatus = "owner status"
)

type Inbox struct{}
type Waiting struct{}
type Status struct{}
type OwnerStatus struct {
	Channel string `json:"channel,omitempty"`
}

func (Inbox) command()       {}
func (Waiting) command()     {}
func (Status) command()      {}
func (OwnerStatus) command() {}

func (Inbox) Verb() string       { return VerbInbox }
func (Waiting) Verb() string     { return VerbWaiting }
func (Status) Verb() string      { return VerbStatus }
func (OwnerStatus) Verb() string { return VerbOwnerStatus }

func decodeLedgerCommand[T Command](args json.RawMessage) (Command, error) {
	var cmd T
	if err := strictUnmarshal(args, &cmd); err != nil {
		return nil, err
	}
	return cmd, nil
}

type InboxResult struct {
	Unread []InboxItem `json:"unread"`
	Owed   []InboxItem `json:"owed"`
	// Cancelled lists requests the owner cancelled before the member read
	// them. They are not unread work: no answer is owed.
	Cancelled []InboxItem `json:"cancelled,omitempty"`
	// Delivers reports that the hall can publish new work to this member
	// now, so an empty inbox needs no wait.
	Delivers bool `json:"delivers,omitempty"`
}

type MemberInboxSummary struct {
	Pending   []int64 `json:"pending"`
	OwedCount int     `json:"owed_count"`
}

type InboxItem struct {
	ID      int64  `json:"id"`
	Kind    string `json:"kind"`
	Sender  string `json:"sender"`
	DueAtMS *int64 `json:"due_at_ms"`
}

type WaitingResult struct {
	GeneratedAtMS int64           `json:"generated_at_ms"`
	Channel       string          `json:"channel"`
	Requests      []LedgerRequest `json:"requests"`
}

type StatusResult struct {
	GeneratedAtMS int64           `json:"generated_at_ms"`
	Self          *StatusSelf     `json:"self,omitempty"`
	Channels      []ChannelStatus `json:"channels"`
}

// StatusSelf reports the calling member's rounds.
type StatusSelf struct {
	Alias      string          `json:"alias"`
	Round      int64           `json:"round"`
	RoundLimit int64           `json:"round_limit"`
	Exchanges  []ExchangeRound `json:"exchanges"`
}

type ChannelStatus struct {
	ID         int64           `json:"id"`
	Name       string          `json:"name"`
	Bindings   []StatusBinding `json:"bindings"`
	Requests   []LedgerRequest `json:"requests"`
	Exchanges  []ExchangeRound `json:"exchanges"`
	GuardStops []GuardStop     `json:"guard_stops"`
}

type StatusBinding struct {
	ID          int64  `json:"id"`
	PrincipalID int64  `json:"principal_id"`
	Alias       string `json:"alias"`
	State       string `json:"state"`
}

type LedgerRequest struct {
	ID         int64             `json:"id"`
	SenderID   int64             `json:"sender_id"`
	Sender     string            `json:"sender"`
	ExchangeID int64             `json:"exchange_id"`
	DueAtMS    int64             `json:"due_at_ms"`
	Recipients []LedgerRecipient `json:"recipients"`
}

type LedgerRecipient struct {
	PrincipalID       int64     `json:"principal_id"`
	BindingID         *int64    `json:"binding_id"`
	Alias             string    `json:"alias"`
	State             string    `json:"state"`
	BindingState      string    `json:"binding_state,omitempty"`
	StoredAtMS        int64     `json:"stored_at_ms"`
	PickedUpAtMS      *int64    `json:"picked_up_at_ms"`
	SettledAtMS       *int64    `json:"settled_at_ms"`
	MoreTimeAtMS      *int64    `json:"more_time_at_ms"`
	OverdueSinceMS    *int64    `json:"overdue_since_ms"`
	WasOverdueSinceMS *int64    `json:"was_overdue_since_ms,omitempty"`
	LateByMS          *int64    `json:"late_by_ms"`
	ReplyUnread       bool      `json:"reply_unread,omitempty"`
	Receipts          []Receipt `json:"receipts"`
	// Observed is the recipient's last adapter fact, for an open obligation
	// only: what it was last seen doing, with no judgement attached. It is
	// in absolute time, so the ledger does not change with the clock.
	Observed *ObservedFact `json:"observed,omitempty"`
	// Notice is the publish notice that offered this request, if any, and
	// ReplyNotice the one that offered the asker this recipient's reply.
	Notice      *LedgerNotice `json:"notice,omitempty"`
	ReplyNotice *LedgerNotice `json:"reply_notice,omitempty"`
}

type Receipt struct {
	Stage    string `json:"stage"`
	AtMS     int64  `json:"at_ms"`
	Evidence string `json:"evidence"`
}

type ExchangeRound struct {
	ID              int64 `json:"id"`
	Round           int64 `json:"round"`
	RoundLimit      int64 `json:"round_limit"`
	ParentRequestID int64 `json:"parent_request_id,omitempty"`
}

type GuardStop struct {
	ExchangeID   int64    `json:"exchange_id,omitempty"`
	Alias        string   `json:"alias,omitempty"`
	BindingID    int64    `json:"binding_id,omitempty"`
	BindingState string   `json:"binding_state,omitempty"`
	AtMS         int64    `json:"at_ms"`
	Round        int64    `json:"round"`
	RoundLimit   int64    `json:"round_limit"`
	Sender       string   `json:"sender"`
	Recipients   []string `json:"recipients"`
	// Requests are a thread's requests since its last reset; past
	// maxStopRequests it keeps the first two and the latest ones, and
	// RequestCount counts them all.
	Requests      []StopRequest `json:"requests,omitempty"`
	RequestCount  int64         `json:"request_count,omitempty"`
	LastResetAtMS *int64        `json:"last_reset_at_ms,omitempty"`
}

type StopRequest struct {
	ID         int64    `json:"id"`
	Sender     string   `json:"sender"`
	Recipients []string `json:"recipients"`
}

func (s *Service) inbox(ctx context.Context, tx store.Tx, caller core.Caller, now time.Time) (core.Result, error) {
	member, ok, err := lookupLedgerMember(ctx, tx, caller)
	if err != nil {
		return core.Result{}, err
	}
	if !ok || caller.Role != core.RoleMember {
		return notEnrolled(), nil
	}
	out, err := inboxItems(ctx, tx, member, false)
	if err != nil {
		return core.Result{}, err
	}
	if out.Delivers, err = publishDelivers(ctx, tx, member.binding, now); err != nil {
		return core.Result{}, err
	}
	if err := observe(ctx, tx, member.principal, now); err != nil {
		return core.Result{}, err
	}
	return OK(out)
}

const notMoreTime = `m.outcome IS NOT '` + OutcomeMoreTime + `'`

// An unread owner cancellation stays visible until read, while the
// recipient's own final reply clears the ask.
const visibleInInbox = `COALESCE(o.state, 'open') IN ('open', 'cancelled') AND ` + notMoreTime

// inboxItems serves both the inbox command and read-only wait snapshots.
func inboxItems(ctx context.Context, tx store.Tx, member memberView, workOnly bool) (InboxResult, error) {
	rows, err := tx.QueryContext(ctx, `SELECT m.id, m.kind, COALESCE(b.alias, ''), m.due_at_ms, r.picked_up_at_ms IS NULL,
			COALESCE(o.state = 'cancelled', 0)
		FROM recipient r JOIN message m ON m.id = r.message_id
		LEFT JOIN binding b ON b.channel_id = m.channel_id AND b.principal_id = m.sender_id
		LEFT JOIN obligation o ON o.message_id = r.message_id AND o.principal_id = r.principal_id
		WHERE r.principal_id = ? AND m.channel_id = ? AND `+visibleInInbox+`
			AND (r.picked_up_at_ms IS NULL OR o.state = 'open')
			AND (NOT ? OR m.kind <> 'note')
		ORDER BY m.id`, member.principal, member.channel, workOnly)
	if err != nil {
		return InboxResult{}, err
	}
	defer rows.Close()
	out := InboxResult{Unread: []InboxItem{}, Owed: []InboxItem{}}
	for rows.Next() {
		var item InboxItem
		var unread, cancelled bool
		if err := rows.Scan(&item.ID, &item.Kind, &item.Sender, &item.DueAtMS, &unread, &cancelled); err != nil {
			return InboxResult{}, err
		}
		if unread && cancelled {
			out.Cancelled = append(out.Cancelled, item)
		} else if unread {
			out.Unread = append(out.Unread, item)
		} else {
			out.Owed = append(out.Owed, item)
		}
	}
	return out, rows.Err()
}

func (s *Service) waiting(ctx context.Context, tx store.Tx, caller core.Caller, now time.Time) (core.Result, error) {
	member, ok, err := lookupLedgerMember(ctx, tx, caller)
	if err != nil {
		return core.Result{}, err
	}
	if !ok || caller.Role != core.RoleMember {
		return notEnrolled(), nil
	}
	requests, err := ledgerRequests(ctx, tx, member.channel, member.principal, false, now)
	if err != nil {
		return core.Result{}, err
	}
	if err := observe(ctx, tx, member.principal, now); err != nil {
		return core.Result{}, err
	}
	return OK(WaitingResult{GeneratedAtMS: ms(now), Channel: member.name, Requests: requests})
}

func (s *Service) status(ctx context.Context, tx store.Tx, caller core.Caller, now time.Time) (core.Result, error) {
	member, ok, err := lookupLedgerMember(ctx, tx, caller)
	if err != nil {
		return core.Result{}, err
	}
	if !ok || caller.Role != core.RoleMember {
		return notEnrolled(), nil
	}
	ch, err := channelLedger(ctx, tx, channelRow{id: member.channel, name: member.name}, false, now)
	if err != nil {
		return core.Result{}, err
	}
	self, err := statusSelf(ctx, tx, member, now)
	if err != nil {
		return core.Result{}, err
	}
	if err := observe(ctx, tx, member.principal, now); err != nil {
		return core.Result{}, err
	}
	return OK(StatusResult{GeneratedAtMS: ms(now), Self: &self, Channels: []ChannelStatus{ch}})
}

func statusSelf(ctx context.Context, tx store.Tx, member memberView, now time.Time) (StatusSelf, error) {
	out := StatusSelf{Exchanges: []ExchangeRound{}}
	if err := tx.QueryRowContext(ctx, `SELECT alias FROM binding WHERE id = ?`, member.binding).Scan(&out.Alias); err != nil {
		return out, err
	}
	level, limit, err := guardPosition(ctx, tx, member.principal, 0, 0)
	if err != nil {
		return out, err
	}
	out.Round, out.RoundLimit = roundAt(level, ms(now)), limit
	rows, err := tx.QueryContext(ctx, `WITH positions AS (
        SELECT thread_root_id, level_ms FROM thread_position WHERE principal_id = ?1
        UNION ALL
        SELECT root.id, ask.level_ms FROM obligation o JOIN message ask ON ask.id = o.message_id
        JOIN message root ON root.id = COALESCE(ask.thread_root_id, ask.id)
        WHERE o.principal_id = ?1 AND o.state = 'open' AND ask.channel_id = ?2
          AND ask.id > root.thread_reset_after_message_id
    )
    SELECT e.id, MAX(p.level_ms), MAX(root.thread_round_limit)
    FROM positions p JOIN message root ON root.id = p.thread_root_id JOIN exchange e ON e.id = root.exchange_id
    WHERE e.channel_id = ?2 AND p.level_ms > ?3
    GROUP BY e.id ORDER BY MAX(p.level_ms) DESC, e.id LIMIT 3`, member.principal, member.channel, ms(now))
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var round ExchangeRound
		var level int64
		if err := rows.Scan(&round.ID, &level, &round.RoundLimit); err != nil {
			return out, err
		}
		round.Round = roundAt(level, ms(now))
		out.Exchanges = append(out.Exchanges, round)
	}
	return out, rows.Err()
}

func (s *Service) ownerStatus(ctx context.Context, tx store.Tx, caller core.Caller, cmd OwnerStatus, now time.Time) (core.Result, error) {
	if caller.Role != core.RoleOwner {
		return Refuse(core.CodeOwnerOnly, "the full ledger requires the owner", "crosshall status"), nil
	}
	if cmd.Channel != "" && !ValidName(cmd.Channel) {
		return Fail(core.ExitUsage, "", "invalid channel name", "crosshall owner status --all lists the channels"), nil
	}
	channels := []channelRow{}
	rows, err := tx.QueryContext(ctx, "SELECT id, name FROM channel WHERE (? = '' OR name = ?) ORDER BY id", cmd.Channel, cmd.Channel)
	if err != nil {
		return core.Result{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var ch channelRow
		if err := rows.Scan(&ch.id, &ch.name); err != nil {
			return core.Result{}, err
		}
		channels = append(channels, ch)
	}
	if err := rows.Err(); err != nil {
		return core.Result{}, err
	}
	if cmd.Channel != "" && len(channels) == 0 {
		return Refuse(core.CodeNotFound, fmt.Sprintf("channel #%s was not found", cmd.Channel), "crosshall owner status --all"), nil
	}
	out := StatusResult{GeneratedAtMS: ms(now), Channels: make([]ChannelStatus, 0, len(channels))}
	for _, ch := range channels {
		status, err := channelLedger(ctx, tx, ch, true, now)
		if err != nil {
			return core.Result{}, err
		}
		out.Channels = append(out.Channels, status)
	}
	return OK(out)
}

func channelLedger(ctx context.Context, tx store.Tx, ch channelRow, owner bool, now time.Time) (ChannelStatus, error) {
	out := ChannelStatus{ID: ch.id, Name: ch.name, Bindings: []StatusBinding{}, Exchanges: []ExchangeRound{}, GuardStops: []GuardStop{}}
	var err error
	out.Requests, err = ledgerRequests(ctx, tx, ch.id, 0, owner, now)
	if err != nil || !owner {
		return out, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT e.id, COALESCE(e.parent_request_id, 0), MAX(root.thread_round_limit),
        MAX(CASE WHEN ask.id > root.thread_reset_after_message_id THEN ask.level_ms ELSE 0 END) AS level
        FROM message ask JOIN message root ON root.id = COALESCE(ask.thread_root_id, ask.id)
        JOIN exchange e ON e.id = root.exchange_id
        WHERE ask.channel_id = ? AND ask.kind = 'ask'
        GROUP BY e.id HAVING level > ? OR e.episode_open = 1 ORDER BY e.id`, ch.id, ms(now))
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var round ExchangeRound
		var level int64
		if err := rows.Scan(&round.ID, &round.ParentRequestID, &round.RoundLimit, &level); err != nil {
			return out, err
		}
		round.Round = roundAt(level, ms(now))
		out.Exchanges = append(out.Exchanges, round)
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	bindings, err := tx.QueryContext(ctx, "SELECT id, principal_id, alias, state FROM binding WHERE channel_id = ? ORDER BY id", ch.id)
	if err != nil {
		return out, err
	}
	defer bindings.Close()
	for bindings.Next() {
		var b StatusBinding
		if err := bindings.Scan(&b.ID, &b.PrincipalID, &b.Alias, &b.State); err != nil {
			return out, err
		}
		out.Bindings = append(out.Bindings, b)
	}
	if err := bindings.Err(); err != nil {
		return out, err
	}
	events, err := tx.QueryContext(ctx, `WITH stopped(subject, exchange_id, alias, binding_id, binding_state) AS (
        SELECT 'exchange:' || id, id, '', 0, '' FROM exchange WHERE channel_id = ?1 AND episode_open = 1
        UNION ALL
        SELECT 'principal:' || p.id, 0, b.alias, b.id, b.state FROM principal p JOIN binding b ON b.principal_id = p.id
        WHERE b.channel_id = ?1 AND p.guard_told = 1
    )
    SELECT stopped.exchange_id, stopped.alias, stopped.binding_id, stopped.binding_state, e.at_ms, json_extract(e.detail, '$.round'),
        json_extract(e.detail, '$.round_limit'), COALESCE(json_extract(e.detail, '$.sender'), ''),
        COALESCE(json_extract(e.detail, '$.recipients'), '[]'),
        (SELECT at_ms FROM event WHERE kind = ?3 AND subject = stopped.subject ORDER BY seq DESC LIMIT 1)
    FROM stopped JOIN event e ON e.seq = (
        SELECT MAX(seq) FROM event WHERE kind = ?2 AND subject = stopped.subject)
    ORDER BY e.seq`, ch.id, guardStopped, guardResetKind)
	if err != nil {
		return out, err
	}
	defer events.Close()
	for events.Next() {
		var stop GuardStop
		var recipients string
		if err := events.Scan(&stop.ExchangeID, &stop.Alias, &stop.BindingID, &stop.BindingState, &stop.AtMS, &stop.Round, &stop.RoundLimit,
			&stop.Sender, &recipients, &stop.LastResetAtMS); err != nil {
			return out, err
		}
		if err := json.Unmarshal([]byte(recipients), &stop.Recipients); err != nil {
			return out, err
		}
		out.GuardStops = append(out.GuardStops, stop)
	}
	if err := events.Err(); err != nil {
		return out, err
	}
	for i := range out.GuardStops {
		if stop := &out.GuardStops[i]; stop.ExchangeID != 0 {
			if stop.Requests, stop.RequestCount, err = stopRequests(ctx, tx, ch.id, stop.ExchangeID); err != nil {
				return out, err
			}
		}
	}
	return out, nil
}

const maxStopRequests = 50

// stopRequests lists the requests on an exchange's threads since each
// thread's last reset. An exchange from before the cutover may have several
// roots; their requests are listed together.
func stopRequests(ctx context.Context, tx store.Tx, channel, exchange int64) ([]StopRequest, int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT m.id, COALESCE(sb.alias, ''), (SELECT json_group_array(alias_at_commit)
        FROM (SELECT alias_at_commit FROM recipient WHERE message_id = m.id ORDER BY alias_at_commit))
        FROM message m JOIN message root ON root.id = COALESCE(m.thread_root_id, m.id)
        LEFT JOIN binding sb ON sb.channel_id = m.channel_id AND sb.principal_id = m.sender_id
        WHERE m.channel_id = ? AND m.kind = 'ask' AND root.exchange_id = ? AND m.id > root.thread_reset_after_message_id
        ORDER BY m.id`, channel, exchange)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []StopRequest{}
	for rows.Next() {
		var r StopRequest
		var recipients string
		if err := rows.Scan(&r.ID, &r.Sender, &recipients); err != nil {
			return nil, 0, err
		}
		if err := json.Unmarshal([]byte(recipients), &r.Recipients); err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	count := int64(len(out))
	if len(out) > maxStopRequests {
		out = append(out[:2], out[len(out)-maxStopRequests+2:]...)
	}
	return out, count, rows.Err()
}

// A nonzero waitingFor scopes the view to an asker's unsettled or unread work.
func ledgerRequests(ctx context.Context, tx store.Tx, channel, waitingFor int64, owner bool, now time.Time) ([]LedgerRequest, error) {
	rows, err := tx.QueryContext(ctx, `SELECT m.id, m.sender_id, COALESCE(sb.alias, ''), m.exchange_id, m.due_at_ms, m.created_at_ms,
		r.principal_id, b.id, r.alias_at_commit, o.state, COALESCE(b.state, 'departed'), r.picked_up_at_ms, o.settled_at_ms, o.more_time_at_ms,
		EXISTS(SELECT 1 FROM message p JOIN recipient pr ON pr.message_id = p.id AND pr.principal_id = m.sender_id
			WHERE p.in_reply_to = m.id AND p.kind = 'reply' AND p.outcome IS NOT '`+OutcomeMoreTime+`'
				AND p.sender_id = r.principal_id AND pr.picked_up_at_ms IS NULL),
		(SELECT json_group_array(json_object('stage', stage, 'at_ms', at_ms, 'evidence', evidence))
		 FROM (SELECT stage, at_ms, evidence FROM receipt WHERE message_id = m.id AND principal_id = r.principal_id
			ORDER BY at_ms, stage, seq))
		FROM message m JOIN recipient r ON r.message_id = m.id
		JOIN obligation o ON o.message_id = m.id AND o.principal_id = r.principal_id
		LEFT JOIN binding b ON b.channel_id = m.channel_id AND b.principal_id = r.principal_id
		LEFT JOIN binding sb ON sb.channel_id = m.channel_id AND sb.principal_id = m.sender_id
		WHERE m.channel_id = ? AND m.kind = 'ask' AND (? = 0 OR (m.sender_id = ? AND (
			EXISTS(SELECT 1 FROM obligation q WHERE q.message_id = m.id AND q.state = 'open')
			OR EXISTS(SELECT 1 FROM message p JOIN recipient pr ON pr.message_id = p.id AND pr.principal_id = m.sender_id
				WHERE p.in_reply_to = m.id AND p.kind = 'reply' AND p.outcome IS NOT '`+OutcomeMoreTime+`'
					AND pr.picked_up_at_ms IS NULL)
		))) ORDER BY m.id, r.principal_id`, channel, waitingFor, waitingFor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LedgerRequest{}
	for rows.Next() {
		var m LedgerRequest
		var r LedgerRecipient
		var receipts, bindingState string
		if err := rows.Scan(&m.ID, &m.SenderID, &m.Sender, &m.ExchangeID, &m.DueAtMS, &r.StoredAtMS,
			&r.PrincipalID, &r.BindingID, &r.Alias, &r.State, &bindingState, &r.PickedUpAtMS, &r.SettledAtMS, &r.MoreTimeAtMS,
			&r.ReplyUnread, &receipts); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(receipts), &r.Receipts); err != nil {
			return nil, err
		}
		if !owner {
			r.BindingID = nil
		}
		if waitingFor == 0 {
			r.ReplyUnread = false
		}
		r.setDeadlineState(m.DueAtMS, ms(now))
		if r.Notice, err = ledgerNotice(ctx, tx, r.PrincipalID, m.ID, 0, false); err != nil {
			return nil, err
		}
		if r.ReplyNotice, err = ledgerNotice(ctx, tx, m.SenderID, m.ID, r.PrincipalID, true); err != nil {
			return nil, err
		}
		if r.State == "open" {
			if bindingState == "departed" {
				r.BindingState = bindingState
			}
			if r.Observed, err = latestFact(ctx, tx, r.PrincipalID); err != nil {
				return nil, err
			}
		}
		if len(out) == 0 || out[len(out)-1].ID != m.ID {
			out = append(out, m)
		}
		last := &out[len(out)-1]
		last.Recipients = append(last.Recipients, r)
	}
	return out, rows.Err()
}

// LedgerNotice is a publish notice as waiting shows it, read from the
// attempt's durable items: when it passed the final fence, when the
// transport's outcome was recorded, and how the member's next prompt was
// observed to carry it.
type LedgerNotice struct {
	ID             int64  `json:"id"`
	State          string `json:"state"`
	SubmittingAtMS *int64 `json:"submitting_at_ms"`
	ResolvedAtMS   *int64 `json:"resolved_at_ms"`
	Observation    string `json:"observation,omitempty"`
	ObservedAtMS   *int64 `json:"observed_at_ms"`
}

// ledgerNotice reads the latest notice that offered request message to
// principal or, with reply set, the latest that offered principal a final
// reply from replier to request message.
func ledgerNotice(ctx context.Context, tx store.Tx, principal, message, replier int64, reply bool) (*LedgerNotice, error) {
	var n LedgerNotice
	var observation sql.NullString
	err := tx.QueryRowContext(ctx, `SELECT a.id, a.state, a.submitting_at_ms, a.resolved_at_ms, a.observation, a.observed_at_ms
		FROM publish_attempt_item i JOIN publish_attempt a ON a.id = i.attempt_id
		WHERE i.principal_id = ?1 AND (NOT ?4 AND i.message_id = ?2 OR ?4 AND i.message_id IN (SELECT p.id FROM message p
			WHERE p.in_reply_to = ?2 AND p.kind = 'reply' AND p.outcome IN ('answer', 'decline') AND p.sender_id = ?3))
		ORDER BY a.id DESC LIMIT 1`, principal, message, replier, reply).
		Scan(&n.ID, &n.State, &n.SubmittingAtMS, &n.ResolvedAtMS, &observation, &n.ObservedAtMS)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	n.Observation = observation.String
	return &n, err
}

func (r *LedgerRecipient) setDeadlineState(due, now int64) {
	if r.State == "open" && now > due {
		r.OverdueSinceMS = &due
	}
	if r.SettledAtMS != nil && *r.SettledAtMS > due {
		switch r.State {
		case "replied", "declined":
			late := *r.SettledAtMS - due
			r.LateByMS = &late
		case "cancelled":
			r.WasOverdueSinceMS = &due
		}
	}
}

func withMemberOutput(ctx context.Context, tx store.Tx, caller core.Caller, req Request, result core.Result, now time.Time) (core.Result, error) {
	joined := req.Command.Verb() == VerbJoin && result.Status == core.ExitOK
	if caller.Role != core.RoleMember && !joined {
		return result, nil
	}
	switch req.Command.(type) {
	case HookEnroll, HookEvent:
		return result, nil // a hook prints only its own presentation
	}
	member, ok, err := lookupLedgerMember(ctx, tx, caller)
	if err != nil {
		return core.Result{}, err
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(result.Data, &fields); err != nil {
		return core.Result{}, err
	}
	if joined && req.Vendor == "agy" && fields["hint"] != nil && caller.PrincipalID != nil {
		// A keyed replay keeps its original hint even after the member leaves.
		// Check the binding that survives both departure and leaf recreation.
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
			SELECT 1 FROM binding WHERE principal_id = ? AND hint_acked = 1)`,
			*caller.PrincipalID).Scan(&result.JoinHintAcked); err != nil {
			return core.Result{}, err
		}
	}
	// A successful leave brings the summary it saved before departure.
	if ok {
		summary, err := memberInboxSummary(ctx, tx, member.principal, member.channel)
		if err != nil {
			return core.Result{}, err
		}
		fields["pending"], _ = json.Marshal(summary.Pending)
		fields["owed_count"], _ = json.Marshal(summary.OwedCount)
	} else if fields["pending"] == nil {
		fields["pending"] = json.RawMessage(`[]`)
	}
	if joined && fields["owed_count"] == nil {
		fields["owed_count"] = json.RawMessage(`0`)
	}
	if fields["roster_changes"] == nil {
		fields["roster_changes"] = json.RawMessage(`[]`)
	}
	if ok && result.Status == core.ExitOK && member.hintAcked &&
		req.Command.Verb() != VerbJoin && req.Command.Verb() != VerbWho {
		seq, err := rosterSeq(ctx, tx, member.channel)
		if err != nil {
			return core.Result{}, err
		}
		changes, err := rosterChanges(ctx, tx, member.channel, member.cursor, member.principal, now)
		if err != nil {
			return core.Result{}, err
		}
		fields["roster_seq"], _ = json.Marshal(seq)
		if len(changes) > 0 {
			fields["roster_changes"], err = json.Marshal(changes)
			if err != nil {
				return core.Result{}, err
			}
		}
	}
	if ok && result.Status == core.ExitOK && !member.hintAcked && req.Command.Verb() != VerbJoin {
		if err := bootstrapRoster(ctx, tx, member, fields, now); err != nil {
			return core.Result{}, err
		}
	}
	result.Data, err = json.Marshal(fields)
	return result, err
}

// bootstrapRoster serves a member that a hook enrolled: the hook told it at
// most who it is, so its first ordinary command carries the member rules and
// the changes since the snapshot the hook showed. The CLI acknowledges after
// rendering, which ends the bootstrap.
func bootstrapRoster(ctx context.Context, tx store.Tx, member memberView, fields map[string]json.RawMessage, now time.Time) error {
	var baseline sql.NullInt64
	var alias string
	if err := tx.QueryRowContext(ctx, `SELECT roster_baseline_seq, alias FROM binding WHERE id = ?`, member.binding).
		Scan(&baseline, &alias); err != nil {
		return err
	}
	if !baseline.Valid {
		return nil
	}
	seq, err := rosterSeq(ctx, tx, member.channel)
	if err != nil {
		return err
	}
	changes, err := rosterChanges(ctx, tx, member.channel, uint64(baseline.Int64), member.principal, now)
	if err != nil {
		return err
	}
	if changes == nil {
		changes = []Member{}
	}
	fields["join_hint"], _ = json.Marshal(Hint(alias, member.name))
	fields["rules_given"] = json.RawMessage(`true`)
	fields["roster_seq"], _ = json.Marshal(seq)
	fields["roster_changes"], err = json.Marshal(changes)
	return err
}

// An unread note, reply, or open ask is eligible for a pending notice.
const noClosedObligation = `NOT EXISTS (SELECT 1 FROM obligation oc
	WHERE oc.message_id = r.message_id AND oc.principal_id = r.principal_id AND oc.state <> 'open')`

// pendingIDs lists the unread messages still awaiting the member's attention.
func pendingIDs(ctx context.Context, tx store.Tx, principal, channel int64) ([]int64, error) {
	ids, err := selectIDs(ctx, tx, `SELECT m.id FROM recipient r JOIN message m ON m.id = r.message_id
		WHERE r.principal_id = ? AND m.channel_id = ? AND r.picked_up_at_ms IS NULL AND `+notMoreTime+`
			AND `+noClosedObligation+` ORDER BY m.id`, principal, channel)
	if ids == nil {
		ids = []int64{}
	}
	return ids, err
}

func memberInboxSummary(ctx context.Context, tx store.Tx, principal, channel int64) (MemberInboxSummary, error) {
	pending, err := pendingIDs(ctx, tx, principal, channel)
	summary := MemberInboxSummary{Pending: pending}
	if err != nil {
		return summary, err
	}
	err = tx.QueryRowContext(ctx, `SELECT count(*) FROM obligation o
		JOIN recipient r ON r.message_id = o.message_id AND r.principal_id = o.principal_id
		JOIN message m ON m.id = o.message_id
		WHERE o.principal_id = ? AND m.channel_id = ? AND o.state = 'open'
			AND r.picked_up_at_ms IS NOT NULL`, principal, channel).Scan(&summary.OwedCount)
	return summary, err
}
