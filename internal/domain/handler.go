package domain

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// Service implements Handler over the schema version 1 tables.
type Service struct {
	Clock core.Clock
}

var _ Handler = (*Service)(nil)

// Handle runs one command inside the caller's transaction. A refusal is a
// result, not an error; an error means the transaction must be rolled back.
func (s *Service) Handle(ctx context.Context, tx store.Tx, caller core.Caller, req Request) (core.Result, error) {
	now := s.now()
	result, err := s.handleCommand(ctx, tx, caller, req, now)
	if err != nil {
		return core.Result{}, err
	}
	return withMemberOutput(ctx, tx, caller, req, result, now)
}

func (s *Service) handleCommand(ctx context.Context, tx store.Tx, caller core.Caller, req Request, now time.Time) (core.Result, error) {
	switch cmd := req.Command.(type) {
	// read-ledger
	case Read:
		return s.read(ctx, tx, caller, req, cmd, now)
	case Inbox:
		return s.inbox(ctx, tx, caller, now)
	case Waiting:
		return s.waiting(ctx, tx, caller, now)
	case Status:
		return s.status(ctx, tx, caller, now)
	case OwnerStatus:
		return s.ownerStatus(ctx, tx, caller, cmd, now)
	case Cancel:
		return s.cancel(ctx, tx, caller, req, cmd, now)
	case BudgetGrant:
		return s.grantBudget(ctx, tx, caller, req, cmd, now)
		// end read-ledger
	case CreateChannel:
		return s.createChannel(ctx, tx, caller, req, cmd, now)
	case Join:
		return s.join(ctx, tx, caller, req, cmd, now)
	case Who:
		return s.who(ctx, tx, caller, req, now)
	// ask-reply
	case Ask:
		return s.ask(ctx, tx, caller, req, cmd, now)
	case Reply:
		return s.reply(ctx, tx, caller, req, cmd, now)
		// end ask-reply
	}
	return core.Result{}, fmt.Errorf("domain: unsupported command %T", req.Command)
}

func (s *Service) now() time.Time {
	if s.Clock != nil {
		return s.Clock()
	}
	return time.Now()
}

func ms(t time.Time) int64 { return t.UnixMilli() }

// --- operation keys -------------------------------------------------------

// payloadHash fingerprints the verb and every semantic argument.
func payloadHash(cmd Command) (string, error) {
	if n, ok := cmd.(interface{ normalized() Command }); ok {
		cmd = n.normalized()
	}
	raw, err := json.Marshal(cmd)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte(cmd.Verb()+"\x00"), raw...))
	return hex.EncodeToString(sum[:]), nil
}

// replay looks up (principal, key). It returns the stored result when scope
// and payload match, a key_conflict refusal when they do not, or found=false.
func replay(ctx context.Context, tx store.Tx, principal int64, key, scope string, cmd Command) (core.Result, bool, error) {
	if key == "" {
		return core.Result{}, false, nil
	}
	hash, err := payloadHash(cmd)
	if err != nil {
		return core.Result{}, false, err
	}
	var gotScope, gotVerb, gotHash, stored string
	err = tx.QueryRowContext(ctx,
		`SELECT scope, verb, payload_hash, result FROM op WHERE principal_id = ? AND op_key = ?`,
		principal, key).Scan(&gotScope, &gotVerb, &gotHash, &stored)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Result{}, false, nil
	}
	if err != nil {
		return core.Result{}, false, err
	}
	if gotScope != scope || gotVerb != cmd.Verb() || gotHash != hash {
		return Refuse(core.CodeKeyConflict,
			"this operation key was already used for a different command",
			"retry the original command, or drop --key to send a new one"), true, nil
	}
	var r core.Result
	if err := json.Unmarshal([]byte(stored), &r); err != nil {
		return core.Result{}, false, fmt.Errorf("domain: stored result for key: %w", err)
	}
	return r, true, nil
}

func record(ctx context.Context, tx store.Tx, principal int64, key, scope string, cmd Command, r core.Result, now time.Time) error {
	if key == "" {
		return nil
	}
	hash, err := payloadHash(cmd)
	if err != nil {
		return err
	}
	stored, err := json.Marshal(r)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO op (principal_id, op_key, scope, verb, payload_hash, result, at_ms) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		principal, key, scope, cmd.Verb(), hash, string(stored), ms(now))
	return err
}

func audit(ctx context.Context, tx store.Tx, kind, subject string, actor int64, detail any, now time.Time) error {
	raw, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO event (kind, subject, actor_id, detail, at_ms) VALUES (?, ?, ?, ?, ?)`,
		kind, subject, actor, string(raw), ms(now))
	return err
}

func observe(ctx context.Context, tx store.Tx, principal int64, now time.Time) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO observation (principal_id, kind, origin, source, observed_at_ms) VALUES (?, 'command', 'unknown', 'cli', ?)`,
		principal, ms(now))
	return err
}

// --- owner channel create -------------------------------------------------

// ownerPrincipal returns the single owner principal, creating it on first use.
func ownerPrincipal(ctx context.Context, tx store.Tx, now time.Time) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT id FROM principal WHERE kind = 'owner' ORDER BY id LIMIT 1`).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO principal (kind, vendor, root_tokens, created_at_ms) VALUES ('owner', 'owner', 0, ?)`, ms(now))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Service) createChannel(ctx context.Context, tx store.Tx, caller core.Caller, req Request, cmd CreateChannel, now time.Time) (core.Result, error) {
	if caller.Role != core.RoleOwner {
		return Refuse(core.CodeOwnerOnly, "only the owner creates channels",
			"run `crosshall owner channel create` from your own shell"), nil
	}
	if !ValidName(cmd.Name) {
		return Fail(core.ExitUsage, "", fmt.Sprintf("invalid channel name %q", cmd.Name), ""), nil
	}
	if msg := ValidText("topic", cmd.Topic); msg != "" {
		return Fail(core.ExitUsage, "", msg, ""), nil
	}
	owner, err := ownerPrincipal(ctx, tx, now)
	if err != nil {
		return core.Result{}, err
	}
	scope := "channel:" + cmd.Name
	if r, found, err := replay(ctx, tx, owner, req.OpKey, scope, cmd); err != nil || found {
		return r, err
	}
	var exists int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM channel WHERE name = ?`, cmd.Name).Scan(&exists); err != nil {
		return core.Result{}, err
	}
	if exists > 0 {
		return Refuse(core.CodeChannelExists, fmt.Sprintf("#%s already exists", cmd.Name), "crosshall owner who --all"), nil
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO channel (name, topic, state, created_at_ms) VALUES (?, ?, 'active', ?)`,
		cmd.Name, cmd.Topic, ms(now)); err != nil {
		return core.Result{}, err
	}
	if err := audit(ctx, tx, "channel_created", "channel:"+cmd.Name, owner, cmd, now); err != nil {
		return core.Result{}, err
	}
	r, err := OK(ChannelResult{Channel: cmd.Name, Topic: cmd.Topic})
	if err != nil {
		return core.Result{}, err
	}
	return r, record(ctx, tx, owner, req.OpKey, scope, cmd, r, now)
}

// --- join -------------------------------------------------------------------

type channelRow struct {
	id          int64
	name, topic string
	state       string
}

func channelByName(ctx context.Context, tx store.Tx, name string) (channelRow, bool, error) {
	var c channelRow
	err := tx.QueryRowContext(ctx, `SELECT id, name, topic, state FROM channel WHERE name = ?`, name).
		Scan(&c.id, &c.name, &c.topic, &c.state)
	if errors.Is(err, sql.ErrNoRows) {
		return c, false, nil
	}
	return c, err == nil, err
}

func (s *Service) join(ctx context.Context, tx store.Tx, caller core.Caller, req Request, cmd Join, now time.Time) (core.Result, error) {
	if caller.Role == core.RoleOwner {
		return Refuse(core.CodeOwnerOnly, "the owner seat is not available yet", "crosshall owner who --all"), nil
	}
	if cmd.Channel == "" {
		return Fail(core.ExitUsage, "", "name the channel to join: crosshall join <channel>", "crosshall join <channel>"), nil
	}
	if msg := ValidText("focus", cmd.Focus); msg != "" {
		return Fail(core.ExitUsage, "", msg, ""), nil
	}
	if cmd.Alias == "owner" {
		return Refuse(core.CodeAliasTaken, "@owner is reserved", "crosshall join "+cmd.Channel+" --as <alias>"), nil
	}
	ch, ok, err := channelByName(ctx, tx, cmd.Channel)
	if err != nil {
		return core.Result{}, err
	}
	if !ok {
		return s.unknownChannel(ctx, tx, cmd.Channel)
	}
	if ch.state != "active" {
		return Refuse(core.CodeArchived, fmt.Sprintf("#%s is archived", ch.name), ""), nil
	}

	scope := "channel:" + ch.name
	var principal int64
	if caller.PrincipalID != nil {
		principal = *caller.PrincipalID
		if r, found, err := replay(ctx, tx, principal, req.OpKey, scope, cmd); err != nil || found {
			return r, err
		}
	}

	// One channel per principal, counting departed bindings too.
	var existing struct {
		id                  int64
		channel             int64
		alias, state, focus string
		rosterAck           uint64
		hintAcked           bool
	}
	if principal != 0 {
		err := tx.QueryRowContext(ctx,
			`SELECT id, channel_id, alias, state, focus, roster_ack_seq, hint_acked FROM binding WHERE principal_id = ? ORDER BY id LIMIT 1`, principal).
			Scan(&existing.id, &existing.channel, &existing.alias, &existing.state, &existing.focus, &existing.rosterAck, &existing.hintAcked)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return core.Result{}, err
		}
	}
	if existing.id != 0 && existing.channel != ch.id {
		var other string
		if err := tx.QueryRowContext(ctx, `SELECT name FROM channel WHERE id = ?`, existing.channel).Scan(&other); err != nil {
			return core.Result{}, err
		}
		return Refuse(core.CodeOneChannel,
			fmt.Sprintf("this session is bound to #%s; a session joins one channel", other),
			"crosshall who"), nil
	}
	if existing.id != 0 && cmd.Alias != "" && cmd.Alias != existing.alias {
		return Refuse(core.CodeAliasTaken,
			fmt.Sprintf("you are @%s in #%s; an alias cannot change", existing.alias, ch.name), "crosshall who"), nil
	}

	alias := cmd.Alias
	if existing.id != 0 {
		alias = existing.alias
	}
	restored := existing.id != 0 && existing.state == "live"
	if !restored {
		// A new binding, or a departed one coming back: the alias must be free.
		if alias == "" {
			alias, err = generateAlias(ctx, tx, ch.id, req.Vendor)
			if err != nil {
				return core.Result{}, err
			}
		} else if taken, err := aliasLive(ctx, tx, ch.id, alias); err != nil {
			return core.Result{}, err
		} else if taken {
			return Refuse(core.CodeAliasTaken, fmt.Sprintf("@%s is taken in #%s", alias, ch.name),
				fmt.Sprintf("crosshall join %s --as <another alias>", ch.name)), nil
		}
	}

	if principal == 0 {
		if req.Enroll == nil {
			return core.Result{}, errors.New("domain: first join without an enrollment link")
		}
		vendor := req.Vendor
		if vendor == "" {
			vendor = "unknown"
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO principal (kind, vendor, root_tokens, created_at_ms) VALUES ('session', ?, 0, ?)`,
			vendor, ms(now))
		if err != nil {
			return core.Result{}, err
		}
		if principal, err = res.LastInsertId(); err != nil {
			return core.Result{}, err
		}
		if err := req.Enroll(ctx, tx, principal); err != nil {
			return core.Result{}, err
		}
	}

	focus := cmd.Focus
	switch {
	case existing.id == 0:
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO binding (principal_id, channel_id, alias, state, joined_at_ms, focus, focus_at_ms) VALUES (?, ?, ?, 'live', ?, ?, ?)`,
			principal, ch.id, alias, ms(now), focus, ms(now)); err != nil {
			return core.Result{}, err
		}
		if err := rosterChange(ctx, tx, ch.id, principal, "joined"); err != nil {
			return core.Result{}, err
		}
	case !restored:
		if focus == "" {
			focus = existing.focus
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE binding SET state = 'live', departed_at_ms = NULL, joined_at_ms = ?, focus = ?, focus_at_ms = ? WHERE id = ?`,
			ms(now), focus, ms(now), existing.id); err != nil {
			return core.Result{}, err
		}
		if err := rosterChange(ctx, tx, ch.id, principal, "joined"); err != nil {
			return core.Result{}, err
		}
	case focus != "" && focus != existing.focus:
		if _, err := tx.ExecContext(ctx,
			`UPDATE binding SET focus = ?, focus_at_ms = ? WHERE id = ?`, focus, ms(now), existing.id); err != nil {
			return core.Result{}, err
		}
		if err := rosterChange(ctx, tx, ch.id, principal, "focus"); err != nil {
			return core.Result{}, err
		}
	}
	if err := observe(ctx, tx, principal, now); err != nil {
		return core.Result{}, err
	}
	if !restored {
		if err := audit(ctx, tx, "joined", "channel:"+ch.name, principal, map[string]string{"alias": alias}, now); err != nil {
			return core.Result{}, err
		}
	}

	seq, err := rosterSeq(ctx, tx, ch.id)
	if err != nil {
		return core.Result{}, err
	}
	out := JoinResult{Channel: ch.name, Topic: ch.topic, Alias: alias, Restored: restored, RosterSeq: seq}
	// The hint and the full roster repeat until a later command acknowledges
	// them; after that a re-join shows only what changed since the cursor.
	if !restored || !(existing.hintAcked || req.Ack > 0) {
		out.Hint = Hint(alias, ch.name)
		members, err := roster(ctx, tx, ch.id, principal, now)
		if err != nil {
			return core.Result{}, err
		}
		if len(members) > RosterCap {
			out.Omitted = len(members) - RosterCap
			members = members[:RosterCap]
		}
		out.Members = members
	} else {
		changes, err := rosterChanges(ctx, tx, ch.id, effectiveAck(existing.rosterAck, req.Ack, seq), principal, now)
		if err != nil {
			return core.Result{}, err
		}
		out.Changes = changes
	}
	r, err := OK(out)
	if err != nil {
		return core.Result{}, err
	}
	return r, record(ctx, tx, principal, req.OpKey, scope, cmd, r, now)
}

func (s *Service) unknownChannel(ctx context.Context, tx store.Tx, name string) (core.Result, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM channel WHERE state = 'active' ORDER BY name`)
	if err != nil {
		return core.Result{}, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return core.Result{}, err
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		return core.Result{}, err
	}
	msg := fmt.Sprintf("no channel #%s", name)
	hint := "ask the owner to create it: crosshall owner channel create " + name
	if len(names) > 0 {
		msg += "; channels: " + strings.Join(names, ", ")
		hint = "crosshall join " + closest(name, names)
	}
	return Refuse(core.CodeNotFound, msg, hint), nil
}

func aliasLive(ctx context.Context, tx store.Tx, channel int64, alias string) (bool, error) {
	var n int
	err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM binding WHERE channel_id = ? AND alias = ? AND state = 'live'`, channel, alias).Scan(&n)
	return n > 0, err
}

func generateAlias(ctx context.Context, tx store.Tx, channel int64, vendor string) (string, error) {
	base := strings.ToLower(vendor)
	if !ValidName(base) || base == "owner" {
		base = "member"
	}
	for n := 1; ; n++ {
		alias := fmt.Sprintf("%s%d", base, n)
		taken, err := aliasLive(ctx, tx, channel, alias)
		if err != nil || !taken {
			return alias, err
		}
	}
}

func rosterChange(ctx context.Context, tx store.Tx, channel, principal int64, field string) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO roster_change (channel_id, seq, principal_id, field)
		 VALUES (?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM roster_change WHERE channel_id = ?), ?, ?)`,
		channel, channel, principal, field)
	return err
}

// --- who --------------------------------------------------------------------

func (s *Service) who(ctx context.Context, tx store.Tx, caller core.Caller, req Request, now time.Time) (core.Result, error) {
	if caller.Role != core.RoleMember || caller.PrincipalID == nil {
		return notEnrolled(), nil
	}
	var ch channelRow
	var hintAcked bool
	err := tx.QueryRowContext(ctx,
		`SELECT c.id, c.name, c.topic, c.state, b.hint_acked FROM binding b JOIN channel c ON c.id = b.channel_id
		 WHERE b.principal_id = ? AND b.state = 'live' ORDER BY b.id LIMIT 1`, *caller.PrincipalID).
		Scan(&ch.id, &ch.name, &ch.topic, &ch.state, &hintAcked)
	if errors.Is(err, sql.ErrNoRows) {
		return notEnrolled(), nil
	}
	if err != nil {
		return core.Result{}, err
	}
	if err := observe(ctx, tx, *caller.PrincipalID, now); err != nil {
		return core.Result{}, err
	}
	members, err := roster(ctx, tx, ch.id, *caller.PrincipalID, now)
	if err != nil {
		return core.Result{}, err
	}
	seq, err := rosterSeq(ctx, tx, ch.id)
	if err != nil {
		return core.Result{}, err
	}
	// A who carries no hint, so it may not acknowledge the join output on the
	// caller's behalf: until the hint is acknowledged it returns no cursor.
	if !hintAcked && req.Ack == 0 {
		seq = 0
	}
	return OK(WhoResult{Channel: ch.name, Topic: ch.topic, Members: members, RosterSeq: seq})
}

func notEnrolled() core.Result {
	return Fail(core.ExitNotEnrolled, core.CodeNotEnrolled,
		"this session is not in a channel", "crosshall join <channel>")
}

// roster lists the live members of a channel, the caller first, then by join time.
func roster(ctx context.Context, tx store.Tx, channel, self int64, now time.Time) ([]Member, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT b.principal_id, b.alias, p.vendor, b.focus, b.joined_at_ms,
		        (SELECT MAX(o.observed_at_ms) FROM observation o WHERE o.principal_id = b.principal_id)
		 FROM binding b JOIN principal p ON p.id = b.principal_id
		 WHERE b.channel_id = ? AND b.state = 'live'
		 ORDER BY b.joined_at_ms, b.id`, channel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var (
			principal, joined int64
			last              sql.NullInt64
			m                 Member
		)
		if err := rows.Scan(&principal, &m.Alias, &m.Vendor, &m.Focus, &joined, &last); err != nil {
			return nil, err
		}
		seen := joined
		if last.Valid {
			seen = last.Int64
		}
		m.LastCommandAgoS = max((ms(now)-seen)/1000, 0)
		m.State, m.Route, m.Self = "unknown", "pull", principal == self
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Self && !out[j].Self })
	return out, nil
}

// closest returns the candidate with the smallest edit distance to name.
func closest(name string, candidates []string) string {
	best, bestD := candidates[0], -1
	for _, c := range candidates {
		if d := editDistance(name, c); bestD < 0 || d < bestD {
			best, bestD = c, d
		}
	}
	return best
}

func editDistance(a, b string) int {
	prev := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(a); i++ {
		cur := make([]int, len(b)+1)
		cur[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev = cur
	}
	return prev[len(b)]
}

// rosterSeq is the channel's latest roster change sequence number.
func rosterSeq(ctx context.Context, tx store.Tx, channel int64) (uint64, error) {
	var seq uint64
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(seq), 0) FROM roster_change WHERE channel_id = ?`, channel).Scan(&seq)
	return seq, err
}

// effectiveAck is the cursor the caller has seen: the stored one, or the one
// this request carries if it is newer, bounded by the current sequence.
func effectiveAck(stored, incoming, current uint64) uint64 {
	return max(stored, min(incoming, current))
}

// rosterChanges lists members other than self whose rows changed after the
// cursor, one row each with its latest change, oldest change first.
func rosterChanges(ctx context.Context, tx store.Tx, channel int64, after uint64, self int64, now time.Time) ([]Member, error) {
	rows, err := tx.QueryContext(ctx,
		`WITH latest AS (
		     SELECT principal_id, MAX(seq) AS seq FROM roster_change
		     WHERE channel_id = ? AND seq > ? AND principal_id <> ?
		     GROUP BY principal_id)
		 SELECT b.alias, p.vendor, b.focus, b.state, b.joined_at_ms,
		        (SELECT MAX(o.observed_at_ms) FROM observation o WHERE o.principal_id = b.principal_id),
		        rc.field
		 FROM latest l
		 JOIN roster_change rc ON rc.channel_id = ? AND rc.seq = l.seq
		 JOIN binding b ON b.principal_id = l.principal_id AND b.channel_id = ?
		 JOIN principal p ON p.id = l.principal_id
		 ORDER BY l.seq`, channel, after, self, channel, channel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Member
	for rows.Next() {
		var (
			m             Member
			state, change string
			joined        int64
			last          sql.NullInt64
		)
		if err := rows.Scan(&m.Alias, &m.Vendor, &m.Focus, &state, &joined, &last, &change); err != nil {
			return nil, err
		}
		seen := joined
		if last.Valid {
			seen = last.Int64
		}
		m.LastCommandAgoS = max((ms(now)-seen)/1000, 0)
		m.State, m.Route, m.Change = "unknown", "pull", change
		if state != "live" {
			m.Change = "departed"
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
