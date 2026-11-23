package domain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// Wire verbs added in slice 2 by ask-reply.
const (
	VerbAsk   = "ask"
	VerbReply = "reply"
)

// Default and limits of a request's deadline, and the largest body accepted.
const (
	DefaultDue = 30 * time.Minute
	MinDue     = time.Minute
	MaxDue     = 24 * time.Hour
	MaxBody    = 64 << 10
)

// Ask sends a request that each recipient owes an answer to.
type Ask struct {
	To   []string `json:"to,omitempty"`
	All  bool     `json:"all,omitempty"`
	Each bool     `json:"each,omitempty"`
	Body string   `json:"body"`
	Due  string   `json:"due,omitempty"`
	Re   int64    `json:"re,omitempty"`
	New  bool     `json:"new,omitempty"`
}

func (Ask) command() {}

// Verb returns the wire verb.
func (Ask) Verb() string { return VerbAsk }

// normalized is the form the operation key fingerprints: targets sorted and
// without duplicates, so the same request typed in another order matches.
func (a Ask) normalized() Command {
	a.To = sortedUnique(a.To)
	return a
}

// Delivery is the observed state of one recipient of a request.
type Delivery struct {
	Alias        string `json:"alias"`
	State        string `json:"state"`
	PickedUpAtMS int64  `json:"picked_up_at_ms,omitempty"`
	SettledAtMS  int64  `json:"settled_at_ms,omitempty"`
	MoreTimeAtMS int64  `json:"more_time_at_ms,omitempty"`
	// Next is the earlier sentence for a stored recipient, kept for old
	// readers; Wake says the same with its reason.
	Next string `json:"next,omitempty"`
	Wake
}

// AskResult is the data of a committed request.
type AskResult struct {
	ID                     int64      `json:"id"`
	Channel                string     `json:"channel"`
	Exchange               int64      `json:"exchange"`
	NewExchange            bool       `json:"new_exchange"`
	ParentRequestID        int64      `json:"parent_request_id,omitempty"`
	ExchangeOpener         string     `json:"exchange_opener,omitempty"`
	ExchangeOpenerDeparted bool       `json:"exchange_opener_departed,omitempty"`
	DueAtMS                int64      `json:"due_at_ms"`
	Round                  int64      `json:"round"`
	Recipients             []Delivery `json:"recipients"`
}

func sortedUnique(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.TrimPrefix(s, "@")
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

// ParseDue reads a request deadline: a duration such as 90s, 30m or 2h,
// from one minute to one day. Empty means the default.
func ParseDue(s string) (time.Duration, error) {
	if s == "" {
		return DefaultDue, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || strings.ContainsAny(s, ".") {
		return 0, fmt.Errorf("--due takes a duration such as 90s, 30m or 2h")
	}
	if d < MinDue || d > MaxDue {
		return 0, fmt.Errorf("--due must be between 1m and 24h")
	}
	return d, nil
}

// checkAsk validates the arguments that do not depend on the store.
func checkAsk(cmd Ask) *core.Result {
	usage := func(msg string) *core.Result {
		r := Fail(core.ExitUsage, "", msg, "crosshall help ask")
		return &r
	}
	switch {
	case cmd.All && len(cmd.To) > 0:
		return usage("address members by alias or with --all, not both")
	case !cmd.All && len(cmd.To) == 0:
		return usage("name at least one @alias, or --all --each")
	case cmd.Re < 0:
		return usage("--re takes a message number")
	case strings.TrimSpace(cmd.Body) == "":
		return usage("the request has no text")
	case !utf8.ValidString(cmd.Body):
		return usage("the text is not valid UTF-8")
	case len(cmd.Body) > MaxBody:
		return usage(fmt.Sprintf("the text is longer than %d bytes; point at a file instead", MaxBody))
	}
	for _, a := range cmd.To {
		if !ValidName(strings.TrimPrefix(a, "@")) {
			return usage(fmt.Sprintf("invalid alias %q", a))
		}
	}
	if _, err := ParseDue(cmd.Due); err != nil {
		return usage(err.Error())
	}
	if cmd.All && !cmd.Each {
		r := Refuse(core.CodeEachRequired,
			"a request to the whole channel makes every member owe an answer; confirm with --each",
			"crosshall ask --all --each \"...\"")
		return &r
	}
	return nil
}

type target struct {
	principal int64
	alias     string
}

// audience resolves the targets to live members, excluding the sender. Every
// bad target is named in one refusal.
func audience(ctx context.Context, tx store.Tx, m member, cmd Ask) ([]target, *core.Result, error) {
	if cmd.All {
		rows, err := tx.QueryContext(ctx,
			`SELECT principal_id, alias FROM binding WHERE channel_id = ? AND state = 'live' AND principal_id <> ? ORDER BY alias`,
			m.channel, m.principal)
		if err != nil {
			return nil, nil, err
		}
		defer rows.Close()
		var out []target
		for rows.Next() {
			var t target
			if err := rows.Scan(&t.principal, &t.alias); err != nil {
				return nil, nil, err
			}
			out = append(out, t)
		}
		if err := rows.Err(); err != nil {
			return nil, nil, err
		}
		if len(out) == 0 {
			r := Refuse(core.CodeUnknownAlias, fmt.Sprintf("#%s has no other members", m.channelName), "crosshall who")
			return nil, &r, nil
		}
		return out, nil, nil
	}
	var (
		out []target
		bad []InvalidTarget
	)
	for _, alias := range sortedUnique(cmd.To) {
		var t target
		var state string
		err := tx.QueryRowContext(ctx,
			`SELECT principal_id, alias, state FROM binding WHERE channel_id = ? AND alias = ?
			 ORDER BY state = 'live' DESC, id DESC LIMIT 1`, m.channel, alias).Scan(&t.principal, &t.alias, &state)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			bad = append(bad, InvalidTarget{Alias: alias, Reason: "unknown"})
		case err != nil:
			return nil, nil, err
		case state != "live":
			bad = append(bad, InvalidTarget{Alias: alias, Reason: "departed"})
		case t.principal == m.principal:
			bad = append(bad, InvalidTarget{Alias: alias, Reason: "self"})
		default:
			out = append(out, t)
		}
	}
	if len(bad) > 0 {
		r := invalidTargets(m.channelName, bad)
		return nil, &r, nil
	}
	return out, nil, nil
}

// InvalidTarget names one recipient a send was refused for, and why:
// unknown, departed or self.
type InvalidTarget struct {
	Alias  string `json:"alias"`
	Reason string `json:"reason"`
}

// invalidTargets refuses a whole send and lists every bad recipient. The code
// is departed only when every bad recipient has left; otherwise it is
// unknown_alias.
func invalidTargets(channel string, bad []InvalidTarget) core.Result {
	code := core.CodeDeparted
	var parts []string
	for _, b := range bad {
		switch b.Reason {
		case "unknown":
			code = core.CodeUnknownAlias
			parts = append(parts, fmt.Sprintf("@%s is not a member of #%s", b.Alias, channel))
		case "departed":
			parts = append(parts, fmt.Sprintf("@%s left #%s", b.Alias, channel))
		case "self":
			code = core.CodeUnknownAlias
			parts = append(parts, fmt.Sprintf("@%s is you", b.Alias))
		}
	}
	raw, _ := json.Marshal(struct {
		Refusal
		InvalidTargets []InvalidTarget `json:"invalid_targets"`
	}{Refusal{Message: "nothing was sent: " + strings.Join(parts, "; "), Hint: "crosshall who"}, bad})
	return core.Result{Status: core.ExitRefused, Code: code, Data: raw}
}

func (s *Service) ask(ctx context.Context, tx store.Tx, caller core.Caller, req Request, cmd Ask, now time.Time) (core.Result, error) {
	m, refusal, err := liveMember(ctx, tx, caller)
	if err != nil || refusal != nil {
		return deref(refusal), err
	}
	if r := checkAsk(cmd); r != nil {
		return *r, nil
	}
	scope := "channel:" + m.channelName
	if r, found, err := replay(ctx, tx, m.principal, req.OpKey, scope, cmd); err != nil || found {
		if err != nil || r.Status != core.ExitOK {
			return r, err
		}
		var out AskResult
		if err := json.Unmarshal(r.Data, &out); err != nil {
			return core.Result{}, err
		}
		return s.askReceipt(ctx, tx, m.principal, out, now)
	}

	to, refusal, err := audience(ctx, tx, m, cmd)
	if err != nil || refusal != nil {
		return deref(refusal), err
	}
	parent, refusal, err := askParent(ctx, tx, m, cmd.Re)
	if err != nil || refusal != nil {
		return deref(refusal), err
	}
	decision, err := guardForAsk(ctx, tx, m, cmd, len(to), now)
	if err != nil {
		return core.Result{}, err
	}
	if decision.Reason != "" {
		return refuseGuard(ctx, tx, m, cmd, to, decision, now)
	}
	threadRoot := decision.Path
	if cmd.New {
		threadRoot = 0
	}
	if err := endDrainedStop(ctx, tx, m.principal, threadRoot, decision.BaseMS, ms(now)); err != nil {
		return core.Result{}, err
	}
	var exchange, parentRequestID int64
	threadLimit := int64(guardDefaultRounds)
	isNew := threadRoot == 0
	if isNew {
		threadLimit = decision.newThreadLimit
		parentRequestID = parent
		exchange, err = open(ctx, tx, m, parentRequestID)
	} else {
		exchange, err = exchangeForRoot(ctx, tx, threadRoot)
	}
	if err != nil {
		return core.Result{}, err
	}
	due, _ := ParseDue(cmd.Due)
	audienceMode := "explicit"
	if cmd.All {
		audienceMode = "all"
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO message (channel_id, sender_id, kind, exchange_id, audience_mode, due_at_ms, body, created_at_ms, level_ms,
		  thread_root_id, thread_round_limit)
		 VALUES (?, ?, 'ask', ?, ?, ?, ?, ?, ?, NULLIF(?, 0), ?)`,
		m.channel, m.principal, exchange, audienceMode, ms(now.Add(due)), cmd.Body, ms(now), decision.LevelMS, threadRoot, threadLimit)
	if err != nil {
		return core.Result{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return core.Result{}, err
	}
	if isNew {
		if _, err := tx.ExecContext(ctx, `UPDATE exchange SET root_message_id = ? WHERE id = ?`, id, exchange); err != nil {
			return core.Result{}, err
		}
	}
	for _, t := range to {
		if err := deliver(ctx, tx, id, t, true, now); err != nil {
			return core.Result{}, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO obligation (message_id, principal_id) VALUES (?, ?)`, id, t.principal); err != nil {
			return core.Result{}, err
		}
	}
	if err := observe(ctx, tx, m.principal, now); err != nil {
		return core.Result{}, err
	}

	out := AskResult{
		ID: id, Channel: m.channelName, Exchange: exchange, NewExchange: isNew, ParentRequestID: parentRequestID,
		DueAtMS: ms(now.Add(due)), Round: decision.Round,
	}
	r, err := s.askReceipt(ctx, tx, m.principal, out, now)
	if err != nil {
		return core.Result{}, err
	}
	var sent AskResult
	if err := json.Unmarshal(r.Data, &sent); err != nil {
		return core.Result{}, err
	}
	if err := recordWakes(ctx, tx, m.principal, id, sent.Recipients, now); err != nil {
		return core.Result{}, err
	}
	return r, record(ctx, tx, m.principal, req.OpKey, scope, cmd, r, now)
}

// deliver addresses message id to one recipient and records that it is stored.
func deliver(ctx context.Context, tx store.Tx, id int64, t target, owes bool, now time.Time) error {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO recipient (message_id, principal_id, alias_at_commit, owes) VALUES (?, ?, ?, ?)`,
		id, t.principal, t.alias, owes); err != nil {
		return err
	}
	return receipt(ctx, tx, id, t.principal, "stored", "dispatcher", now)
}

func receipt(ctx context.Context, tx store.Tx, msg, principal int64, stage, evidence string, now time.Time) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO receipt (message_id, principal_id, stage, seq, at_ms, evidence)
		 VALUES (?, ?, ?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM receipt WHERE message_id = ? AND principal_id = ? AND stage = ?), ?, ?)`,
		msg, principal, stage, msg, principal, stage, ms(now), evidence)
	return err
}

// deliveries reads the current state of every recipient of a message, and
// for a stored one what will tell it.
func (s *Service) deliveries(ctx context.Context, tx store.Tx, msg int64, now time.Time) ([]Delivery, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT r.alias_at_commit, r.principal_id, r.picked_up_at_ms, o.state, o.settled_at_ms, o.more_time_at_ms,
		        COALESCE(b.state, 'departed'), COALESCE(b.id, 0)
		 FROM recipient r JOIN message m ON m.id = r.message_id
		 LEFT JOIN obligation o ON o.message_id = r.message_id AND o.principal_id = r.principal_id
		 LEFT JOIN binding b ON b.principal_id = r.principal_id AND b.channel_id = m.channel_id
		 WHERE r.message_id = ? ORDER BY r.alias_at_commit`, msg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var (
		out       []Delivery
		stored    []int
		bindings  []int64
		principal []int64
	)
	for rows.Next() {
		var (
			d                         Delivery
			picked, settled, moreTime sql.NullInt64
			state                     sql.NullString
			binding                   string
			bindingID, principalID    int64
		)
		if err := rows.Scan(&d.Alias, &principalID, &picked, &state, &settled, &moreTime, &binding, &bindingID); err != nil {
			return nil, err
		}
		d.PickedUpAtMS, d.SettledAtMS, d.MoreTimeAtMS = picked.Int64, settled.Int64, moreTime.Int64
		switch {
		case state.Valid && state.String != "open":
			d.State = state.String
		case moreTime.Valid:
			d.State = "more_time"
		case picked.Valid:
			d.State = "picked_up"
		case binding != "live":
			// Delivery follows the recipient's binding, not its alias.
			d.State, d.Next = "stored", "held until it rejoins"
		default:
			d.State = "stored"
			stored, bindings, principal = append(stored, len(out)), append(bindings, bindingID), append(principal, principalID)
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i, at := range stored {
		w, err := s.messageWake(ctx, tx, bindings[i], principal[i], msg, now)
		if err != nil {
			return nil, err
		}
		out[at].Wake, out[at].Next = w, "waits for its next crosshall command"
		switch w.Wake {
		case WakeNotice:
			out[at].Next = "notice queued"
		case WakeWait:
			out[at].Next = WaitRunning
		}
	}
	return out, nil
}

// Resolve the opener by principal, since a departed member's alias can be reused.
// A replay refreshes delivery and display names.
func (s *Service) askReceipt(ctx context.Context, tx store.Tx, sender int64, out AskResult, now time.Time) (core.Result, error) {
	if !out.NewExchange {
		err := tx.QueryRowContext(ctx, `SELECT COALESCE(b.alias, ''), COALESCE(b.state = 'departed', 0)
			FROM exchange e LEFT JOIN binding b ON b.principal_id = e.opened_by_id
				AND b.channel_id = e.channel_id AND b.principal_id <> ?
			WHERE e.id = ?`, sender, out.Exchange).Scan(&out.ExchangeOpener, &out.ExchangeOpenerDeparted)
		if err != nil {
			return core.Result{}, err
		}
	}
	d, err := s.deliveries(ctx, tx, out.ID, now)
	if err != nil {
		return core.Result{}, err
	}
	out.Recipients = d
	return OK(out)
}

func deref(r *core.Result) core.Result {
	if r == nil {
		return core.Result{}
	}
	return *r
}
