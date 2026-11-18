package domain

import (
	"context"
	"encoding/json"
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
}

type InboxItem struct {
	ID      int64  `json:"id"`
	Kind    string `json:"kind"`
	Sender  string `json:"sender"`
	DueAtMS *int64 `json:"due_at_ms"`
}

type WaitingResult struct {
	Channel  string          `json:"channel"`
	Requests []LedgerRequest `json:"requests"`
}

type StatusResult struct {
	GeneratedAtMS int64           `json:"generated_at_ms"`
	Channels      []ChannelStatus `json:"channels"`
}

type ChannelStatus struct {
	ID                int64              `json:"id"`
	Name              string             `json:"name"`
	Bindings          []StatusBinding    `json:"bindings"`
	Requests          []LedgerRequest    `json:"requests"`
	Exchanges         []ExchangeBalance  `json:"exchanges"`
	BudgetExhaustions []BudgetExhaustion `json:"budget_exhaustions"`
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
	StoredAtMS        int64     `json:"stored_at_ms"`
	PickedUpAtMS      *int64    `json:"picked_up_at_ms"`
	SettledAtMS       *int64    `json:"settled_at_ms"`
	MoreTimeAtMS      *int64    `json:"more_time_at_ms"`
	OverdueSinceMS    *int64    `json:"overdue_since_ms"`
	WasOverdueSinceMS *int64    `json:"was_overdue_since_ms,omitempty"`
	LateByMS          *int64    `json:"late_by_ms"`
	ReplyUnread       bool      `json:"reply_unread,omitempty"`
	Receipts          []Receipt `json:"receipts"`
}

type Receipt struct {
	Stage    string `json:"stage"`
	AtMS     int64  `json:"at_ms"`
	Evidence string `json:"evidence"`
}

type ExchangeBalance struct {
	ID        int64 `json:"id"`
	Granted   int64 `json:"granted"`
	Spent     int64 `json:"spent"`
	Remaining int64 `json:"remaining"`
}

type BudgetExhaustion struct {
	ExchangeID int64 `json:"exchange_id"`
	Episode    int64 `json:"episode"`
	AtMS       int64 `json:"at_ms"`
}

func (s *Service) inbox(ctx context.Context, tx store.Tx, caller core.Caller, now time.Time) (core.Result, error) {
	member, ok, err := lookupLedgerMember(ctx, tx, caller)
	if err != nil {
		return core.Result{}, err
	}
	if !ok || caller.Role != core.RoleMember {
		return notEnrolled(), nil
	}
	rows, err := tx.QueryContext(ctx, `SELECT m.id, m.kind, COALESCE(b.alias, ''), m.due_at_ms, r.picked_up_at_ms IS NULL
		FROM recipient r JOIN message m ON m.id = r.message_id
		LEFT JOIN binding b ON b.channel_id = m.channel_id AND b.principal_id = m.sender_id
		LEFT JOIN obligation o ON o.message_id = r.message_id AND o.principal_id = r.principal_id
		WHERE r.principal_id = ? AND m.channel_id = ? AND (r.picked_up_at_ms IS NULL OR o.state = 'open')
		ORDER BY m.id`, member.principal, member.channel)
	if err != nil {
		return core.Result{}, err
	}
	defer rows.Close()
	out := InboxResult{Unread: []InboxItem{}, Owed: []InboxItem{}}
	for rows.Next() {
		var item InboxItem
		var unread bool
		if err := rows.Scan(&item.ID, &item.Kind, &item.Sender, &item.DueAtMS, &unread); err != nil {
			return core.Result{}, err
		}
		if unread {
			out.Unread = append(out.Unread, item)
		} else {
			out.Owed = append(out.Owed, item)
		}
	}
	if err := rows.Err(); err != nil {
		return core.Result{}, err
	}
	if err := observe(ctx, tx, member.principal, now); err != nil {
		return core.Result{}, err
	}
	return OK(out)
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
	return OK(WaitingResult{Channel: member.name, Requests: requests})
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
	if err := observe(ctx, tx, member.principal, now); err != nil {
		return core.Result{}, err
	}
	return OK(StatusResult{GeneratedAtMS: ms(now), Channels: []ChannelStatus{ch}})
}

func (s *Service) ownerStatus(ctx context.Context, tx store.Tx, caller core.Caller, cmd OwnerStatus, now time.Time) (core.Result, error) {
	if caller.Role != core.RoleOwner {
		return Refuse(core.CodeOwnerOnly, "the full ledger requires the owner", "crosshall status"), nil
	}
	if cmd.Channel != "" && !ValidName(cmd.Channel) {
		return Fail(core.ExitUsage, "", "invalid channel name", ""), nil
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
	out := ChannelStatus{ID: ch.id, Name: ch.name, Bindings: []StatusBinding{}, Exchanges: []ExchangeBalance{}, BudgetExhaustions: []BudgetExhaustion{}}
	var err error
	out.Requests, err = ledgerRequests(ctx, tx, ch.id, 0, owner, now)
	if err != nil {
		return out, err
	}
	rows, err := tx.QueryContext(ctx, "SELECT id, granted, spent FROM exchange WHERE channel_id = ? ORDER BY id", ch.id)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var balance ExchangeBalance
		if err := rows.Scan(&balance.ID, &balance.Granted, &balance.Spent); err != nil {
			return out, err
		}
		balance.Remaining = balance.Granted - balance.Spent
		out.Exchanges = append(out.Exchanges, balance)
	}
	if err := rows.Err(); err != nil || !owner {
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
	events, err := tx.QueryContext(ctx, `SELECT e.id, row_number() OVER (PARTITION BY e.id ORDER BY ev.seq), ev.at_ms
		FROM event ev JOIN exchange e ON ev.subject = 'exchange:' || e.id
		WHERE ev.kind = 'budget_exhausted' AND e.channel_id = ? ORDER BY e.id, ev.seq`, ch.id)
	if err != nil {
		return out, err
	}
	defer events.Close()
	for events.Next() {
		var e BudgetExhaustion
		if err := events.Scan(&e.ExchangeID, &e.Episode, &e.AtMS); err != nil {
			return out, err
		}
		out.BudgetExhaustions = append(out.BudgetExhaustions, e)
	}
	return out, events.Err()
}

// A nonzero waitingFor scopes the view to an asker's unsettled or unread work.
func ledgerRequests(ctx context.Context, tx store.Tx, channel, waitingFor int64, owner bool, now time.Time) ([]LedgerRequest, error) {
	rows, err := tx.QueryContext(ctx, `SELECT m.id, m.sender_id, COALESCE(sb.alias, ''), m.exchange_id, m.due_at_ms, m.created_at_ms,
		r.principal_id, b.id, r.alias_at_commit, o.state, r.picked_up_at_ms, o.settled_at_ms, o.more_time_at_ms,
		EXISTS(SELECT 1 FROM message p JOIN recipient pr ON pr.message_id = p.id AND pr.principal_id = m.sender_id
			WHERE p.in_reply_to = m.id AND p.kind = 'reply' AND p.sender_id = r.principal_id AND pr.picked_up_at_ms IS NULL),
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
				WHERE p.in_reply_to = m.id AND p.kind = 'reply' AND pr.picked_up_at_ms IS NULL)
		))) ORDER BY m.id, r.principal_id`, channel, waitingFor, waitingFor)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LedgerRequest{}
	for rows.Next() {
		var m LedgerRequest
		var r LedgerRecipient
		var receipts string
		if err := rows.Scan(&m.ID, &m.SenderID, &m.Sender, &m.ExchangeID, &m.DueAtMS, &r.StoredAtMS,
			&r.PrincipalID, &r.BindingID, &r.Alias, &r.State, &r.PickedUpAtMS, &r.SettledAtMS, &r.MoreTimeAtMS,
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
		if len(out) == 0 || out[len(out)-1].ID != m.ID {
			out = append(out, m)
		}
		last := &out[len(out)-1]
		last.Recipients = append(last.Recipients, r)
	}
	return out, rows.Err()
}

func (r *LedgerRecipient) setDeadlineState(due, now int64) {
	if r.State == "open" && now >= due {
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
	member, ok, err := lookupLedgerMember(ctx, tx, caller)
	if err != nil {
		return core.Result{}, err
	}
	pending := []int64{}
	if ok {
		pending, err = selectIDs(ctx, tx, `SELECT m.id FROM recipient r JOIN message m ON m.id = r.message_id
			WHERE r.principal_id = ? AND m.channel_id = ? AND r.picked_up_at_ms IS NULL ORDER BY m.id`, member.principal, member.channel)
		if err != nil {
			return core.Result{}, err
		}
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(result.Data, &fields); err != nil {
		return core.Result{}, err
	}
	fields["pending"], _ = json.Marshal(pending)
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
	result.Data, err = json.Marshal(fields)
	return result, err
}
