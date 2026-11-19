package domain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// Wire verbs added in slice 3 by note-log.
const (
	VerbNote  = "note"
	VerbLog   = "log"
	VerbLeave = "leave"
	VerbFocus = "focus"
)

// Limits of a log page. A page also stops before its encoded messages pass
// MaxLogPageBytes, half the 16 MiB response the client accepts, so the
// envelope, pending IDs and roster changes always fit.
const (
	DefaultLogLimit = 20
	MaxLogLimit     = 200
	MaxLogPageBytes = 8 << 20
)

// Note sends a message that asks for nothing: no obligation, no budget.
type Note struct {
	To   []string `json:"to,omitempty"`
	All  bool     `json:"all,omitempty"`
	Body string   `json:"body"`
}

func (Note) command() {}

// Verb returns the wire verb.
func (Note) Verb() string { return VerbNote }

// normalized sorts and de-duplicates the targets for the operation key.
func (n Note) normalized() Command {
	n.To = sortedUnique(n.To)
	return n
}

// NoteResult is the data of a stored note.
type NoteResult struct {
	ID         int64      `json:"id"`
	Channel    string     `json:"channel"`
	Recipients []Delivery `json:"recipients"`
}

// Log lists the caller's channel history. Since nil means the latest page.
type Log struct {
	Since *int64 `json:"since,omitempty"`
	Limit int    `json:"limit,omitempty"`
}

func (Log) command() {}

// Verb returns the wire verb.
func (Log) Verb() string { return VerbLog }

// LogEntry is one message of the channel history.
type LogEntry struct {
	ID          int64    `json:"id"`
	Kind        string   `json:"kind"`
	Sender      string   `json:"sender"`
	To          []string `json:"to"`
	All         bool     `json:"all,omitempty"`
	InReplyTo   *int64   `json:"in_reply_to,omitempty"`
	Outcome     string   `json:"outcome,omitempty"`
	Exchange    *int64   `json:"exchange_id,omitempty"`
	DueAtMS     *int64   `json:"due_at_ms,omitempty"`
	CreatedAtMS int64    `json:"created_at_ms"`
	Body        string   `json:"body"`
}

// LogResult is one page of history, oldest first. Earlier is the number of
// older messages not shown and Later the number of newer ones; Next is the
// --since value that continues in that direction.
type LogResult struct {
	Channel  string     `json:"channel"`
	Messages []LogEntry `json:"messages"`
	Earlier  int        `json:"earlier"`
	Later    int        `json:"later"`
	Next     *int64     `json:"next_since,omitempty"`
}

// Leave marks the caller's membership departed.
type Leave struct{}

func (Leave) command() {}

// Verb returns the wire verb.
func (Leave) Verb() string { return VerbLeave }

// LeaveResult is the data of a leave. Owed and Waiting count the open
// obligations the member leaves behind, owed by it and owed to it.
type LeaveResult struct {
	Channel string `json:"channel"`
	Alias   string `json:"alias"`
	Owed    int    `json:"owed"`
	Waiting int    `json:"waiting"`
	// The inbox summary and roster changes are taken before departure.
	MemberInboxSummary
	Changes []Member `json:"roster_changes"`
}

// Focus sets the caller's one-line focus.
type Focus struct {
	Text string `json:"text"`
}

func (Focus) command() {}

// Verb returns the wire verb.
func (Focus) Verb() string { return VerbFocus }

// FocusResult is the data of a focus change.
type FocusResult struct {
	Channel string `json:"channel"`
	Alias   string `json:"alias"`
	Focus   string `json:"focus"`
	Changed bool   `json:"changed"`
}

// --- note -------------------------------------------------------------------

func checkNote(cmd Note) *core.Result {
	usage := func(msg string) *core.Result {
		r := Fail(core.ExitUsage, "", msg, "crosshall help")
		return &r
	}
	switch {
	case cmd.All && len(cmd.To) > 0:
		return usage("address members by alias or with --all, not both")
	case !cmd.All && len(cmd.To) == 0:
		return usage("name at least one @alias, or --all")
	case strings.TrimSpace(cmd.Body) == "":
		return usage("the note has no text")
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
	return nil
}

func (s *Service) note(ctx context.Context, tx store.Tx, caller core.Caller, req Request, cmd Note, now time.Time) (core.Result, error) {
	m, refusal, err := liveMember(ctx, tx, caller)
	if err != nil || refusal != nil {
		return deref(refusal), err
	}
	if r := checkNote(cmd); r != nil {
		return *r, nil
	}
	scope := "channel:" + m.channelName
	if r, found, err := replay(ctx, tx, m.principal, req.OpKey, scope, cmd); err != nil || found {
		if err != nil || r.Status != core.ExitOK {
			return r, err
		}
		return withCurrentNoteDelivery(ctx, tx, r)
	}
	to, refusal, err := audience(ctx, tx, m, Ask{To: cmd.To, All: cmd.All})
	if err != nil || refusal != nil {
		return deref(refusal), err
	}
	mode := "explicit"
	if cmd.All {
		mode = "all"
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO message (channel_id, sender_id, kind, audience_mode, body, created_at_ms)
		 VALUES (?, ?, 'note', ?, ?, ?)`, m.channel, m.principal, mode, cmd.Body, ms(now))
	if err != nil {
		return core.Result{}, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return core.Result{}, err
	}
	for _, t := range to {
		if err := deliver(ctx, tx, id, t, false, now); err != nil {
			return core.Result{}, err
		}
	}
	if err := observe(ctx, tx, m.principal, now); err != nil {
		return core.Result{}, err
	}
	out := NoteResult{ID: id, Channel: m.channelName}
	if out.Recipients, err = deliveries(ctx, tx, id); err != nil {
		return core.Result{}, err
	}
	r, err := OK(out)
	if err != nil {
		return core.Result{}, err
	}
	return r, record(ctx, tx, m.principal, req.OpKey, scope, cmd, r, now)
}

func withCurrentNoteDelivery(ctx context.Context, tx store.Tx, r core.Result) (core.Result, error) {
	var out NoteResult
	if err := json.Unmarshal(r.Data, &out); err != nil {
		return core.Result{}, err
	}
	d, err := deliveries(ctx, tx, out.ID)
	if err != nil {
		return core.Result{}, err
	}
	out.Recipients = d
	return OK(out)
}

// --- log --------------------------------------------------------------------

// logLimit checks a page request and returns its limit, the default when unset.
func logLimit(cmd Log) (int, *core.Result) {
	if cmd.Limit < 0 || cmd.Limit > MaxLogLimit {
		r := Fail(core.ExitUsage, "", fmt.Sprintf("--limit takes 1 to %d", MaxLogLimit), "crosshall help")
		return 0, &r
	}
	if cmd.Since != nil && *cmd.Since < 0 {
		r := Fail(core.ExitUsage, "", "--since takes a message number", "crosshall help")
		return 0, &r
	}
	if cmd.Limit == 0 {
		return DefaultLogLimit, nil
	}
	return cmd.Limit, nil
}

func (s *Service) log(ctx context.Context, tx store.Tx, caller core.Caller, cmd Log, now time.Time) (core.Result, error) {
	m, refusal, err := liveMember(ctx, tx, caller)
	if err != nil || refusal != nil {
		return deref(refusal), err
	}
	limit, bad := logLimit(cmd)
	if bad != nil {
		return *bad, nil
	}
	out, err := ChannelLog(ctx, tx, m.channel, cmd.Since, limit)
	if err != nil {
		return core.Result{}, err
	}
	out.Channel = m.channelName
	if err := observe(ctx, tx, m.principal, now); err != nil {
		return core.Result{}, err
	}
	return OK(out)
}

// ChannelLog reads one page of a channel's history, oldest first: the
// messages after since, or the latest ones when since is nil. It reads only
// rows of that channel and records no pickup.
func ChannelLog(ctx context.Context, tx store.Tx, channel int64, since *int64, limit int) (LogResult, error) {
	out := LogResult{Messages: []LogEntry{}}
	var query string
	args := []any{channel}
	if since != nil {
		query = `SELECT id FROM message WHERE channel_id = ? AND id > ? ORDER BY id LIMIT ?`
		args = append(args, *since, limit)
	} else {
		query = `SELECT id FROM (SELECT id FROM message WHERE channel_id = ? ORDER BY id DESC LIMIT ?) ORDER BY id`
		args = append(args, limit)
	}
	ids, err := selectIDs(ctx, tx, query, args...)
	if err != nil {
		return out, err
	}
	entries := make([]LogEntry, len(ids))
	for i, id := range ids {
		if entries[i], err = logEntry(ctx, tx, channel, id); err != nil {
			return out, err
		}
	}
	// Keep whole messages within the byte budget: from the oldest when paging
	// forward, from the newest on the latest page. At least one is kept.
	size, keep := 0, 0
	for keep < len(entries) {
		i := keep
		if since == nil {
			i = len(entries) - 1 - keep
		}
		raw, err := json.Marshal(entries[i])
		if err != nil {
			return out, err
		}
		if keep > 0 && size+len(raw) > MaxLogPageBytes {
			break
		}
		size += len(raw)
		keep++
	}
	if since == nil {
		entries, ids = entries[len(entries)-keep:], ids[len(ids)-keep:]
	} else {
		entries, ids = entries[:keep], ids[:keep]
	}
	out.Messages = append(out.Messages, entries...)
	if len(ids) == 0 {
		if since != nil {
			err = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM message WHERE channel_id = ? AND id <= ?`,
				channel, *since).Scan(&out.Earlier)
		}
		return out, err
	}
	first, last := ids[0], ids[len(ids)-1]
	if err := tx.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM message WHERE channel_id = ? AND id < ?),
		        (SELECT COUNT(*) FROM message WHERE channel_id = ? AND id > ?)`,
		channel, first, channel, last).Scan(&out.Earlier, &out.Later); err != nil {
		return out, err
	}
	switch {
	case out.Later > 0:
		out.Next = &last
	case since == nil && out.Earlier > 0:
		// Older history is read forward from the start, so following the
		// continuations reaches every message.
		start := int64(0)
		out.Next = &start
	}
	return out, nil
}

func logEntry(ctx context.Context, tx store.Tx, channel, id int64) (LogEntry, error) {
	e := LogEntry{ID: id, To: []string{}}
	var mode string
	err := tx.QueryRowContext(ctx,
		`SELECT m.kind, COALESCE(b.alias, ''), m.audience_mode, m.in_reply_to, COALESCE(m.outcome, ''),
		        m.exchange_id, m.due_at_ms, m.created_at_ms, m.body
		 FROM message m LEFT JOIN binding b ON b.channel_id = m.channel_id AND b.principal_id = m.sender_id
		 WHERE m.channel_id = ? AND m.id = ?`, channel, id).
		Scan(&e.Kind, &e.Sender, &mode, &e.InReplyTo, &e.Outcome, &e.Exchange, &e.DueAtMS, &e.CreatedAtMS, &e.Body)
	if err != nil {
		return e, err
	}
	e.All = mode == "all"
	rows, err := tx.QueryContext(ctx,
		`SELECT alias_at_commit FROM recipient WHERE message_id = ? ORDER BY alias_at_commit`, id)
	if err != nil {
		return e, err
	}
	defer rows.Close()
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return e, err
		}
		e.To = append(e.To, a)
	}
	return e, rows.Err()
}

// --- leave and focus --------------------------------------------------------

func (s *Service) leave(ctx context.Context, tx store.Tx, caller core.Caller, req Request, cmd Leave, now time.Time) (core.Result, error) {
	// A retried leave arrives after the binding departed, so replay is
	// looked up by principal before the live-member check.
	if caller.PrincipalID != nil && req.OpKey != "" {
		var name string
		err := tx.QueryRowContext(ctx,
			`SELECT c.name FROM binding b JOIN channel c ON c.id = b.channel_id WHERE b.principal_id = ? ORDER BY b.id LIMIT 1`,
			*caller.PrincipalID).Scan(&name)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return core.Result{}, err
		}
		if err == nil {
			if r, found, err := replay(ctx, tx, *caller.PrincipalID, req.OpKey, "channel:"+name, cmd); err != nil || found {
				return r, err
			}
		}
	}
	m, refusal, err := liveMember(ctx, tx, caller)
	if err != nil || refusal != nil {
		return deref(refusal), err
	}
	// What the member has not seen yet is read while it is still live, and
	// kept in the recorded result so a retried leave shows it too.
	out := LeaveResult{Channel: m.channelName, Alias: m.alias, Changes: []Member{}}
	view, _, err := lookupLedgerMember(ctx, tx, caller)
	if err != nil {
		return core.Result{}, err
	}
	if out.MemberInboxSummary, err = memberInboxSummary(ctx, tx, m.principal, m.channel); err != nil {
		return core.Result{}, err
	}
	if view.hintAcked {
		if out.Changes, err = rosterChanges(ctx, tx, m.channel, view.cursor, m.principal, now); err != nil {
			return core.Result{}, err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE binding SET state = 'departed', departed_at_ms = ? WHERE id = ?`, ms(now), m.binding); err != nil {
		return core.Result{}, err
	}
	if err := rosterChange(ctx, tx, m.channel, m.principal, "departed"); err != nil {
		return core.Result{}, err
	}
	if err := observe(ctx, tx, m.principal, now); err != nil {
		return core.Result{}, err
	}
	if err := audit(ctx, tx, "departed", "channel:"+m.channelName, m.principal, map[string]string{"alias": m.alias}, now); err != nil {
		return core.Result{}, err
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT (SELECT COUNT(*) FROM obligation o JOIN message msg ON msg.id = o.message_id
		         WHERE o.principal_id = ? AND o.state = 'open' AND msg.channel_id = ?),
		        (SELECT COUNT(*) FROM obligation o JOIN message msg ON msg.id = o.message_id
		         WHERE msg.sender_id = ? AND o.state = 'open' AND msg.channel_id = ?)`,
		m.principal, m.channel, m.principal, m.channel).Scan(&out.Owed, &out.Waiting); err != nil {
		return core.Result{}, err
	}
	r, err := OK(out)
	if err != nil {
		return core.Result{}, err
	}
	return r, record(ctx, tx, m.principal, req.OpKey, "channel:"+m.channelName, cmd, r, now)
}

func (s *Service) focus(ctx context.Context, tx store.Tx, caller core.Caller, req Request, cmd Focus, now time.Time) (core.Result, error) {
	m, refusal, err := liveMember(ctx, tx, caller)
	if err != nil || refusal != nil {
		return deref(refusal), err
	}
	if msg := ValidText("focus", cmd.Text); msg != "" {
		return Fail(core.ExitUsage, "", msg, ""), nil
	}
	if strings.TrimSpace(cmd.Text) == "" {
		return Fail(core.ExitUsage, "", "focus needs one line of text", `crosshall focus "..."`), nil
	}
	scope := "channel:" + m.channelName
	if r, found, err := replay(ctx, tx, m.principal, req.OpKey, scope, cmd); err != nil || found {
		return r, err
	}
	var current string
	if err := tx.QueryRowContext(ctx, `SELECT focus FROM binding WHERE id = ?`, m.binding).Scan(&current); err != nil {
		return core.Result{}, err
	}
	out := FocusResult{Channel: m.channelName, Alias: m.alias, Focus: cmd.Text, Changed: current != cmd.Text}
	if out.Changed {
		if _, err := tx.ExecContext(ctx,
			`UPDATE binding SET focus = ?, focus_at_ms = ? WHERE id = ?`, cmd.Text, ms(now), m.binding); err != nil {
			return core.Result{}, err
		}
		if err := rosterChange(ctx, tx, m.channel, m.principal, "focus"); err != nil {
			return core.Result{}, err
		}
	}
	if err := observe(ctx, tx, m.principal, now); err != nil {
		return core.Result{}, err
	}
	r, err := OK(out)
	if err != nil {
		return core.Result{}, err
	}
	return r, record(ctx, tx, m.principal, req.OpKey, scope, cmd, r, now)
}
