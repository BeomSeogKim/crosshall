package domain

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

const VerbOwnerTakeover = "owner takeover"

// OwnerTakeover gives a member's binding to the session that asked for it
// with crosshall join --takeover. The ID is the request's event sequence.
type OwnerTakeover struct {
	ID int64 `json:"id"`
}

func (OwnerTakeover) command()     {}
func (OwnerTakeover) Verb() string { return VerbOwnerTakeover }

// OwnerTakeoverResult says whether the binding moved at once (the asking
// session was enrolled) or waits for the session to run join --takeover
// again before Expires.
type OwnerTakeoverResult struct {
	ID        int64  `json:"id"`
	BindingID int64  `json:"binding_id"`
	Channel   string `json:"channel"`
	Alias     string `json:"alias"`
	Session   string `json:"session"`
	Completed bool   `json:"completed"`
	ExpiresMS int64  `json:"expires_at_ms,omitempty"`
}

// TakeoverRequest is a join --takeover's result: the request the owner
// grants with crosshall owner takeover <id>.
type TakeoverRequest struct {
	ID      int64  `json:"id"`
	Channel string `json:"channel"`
	Alias   string `json:"alias"`
	Session string `json:"session"`
	// AtGrant says the owner's grant completes it: the session was enrolled.
	// Otherwise the session runs join --takeover again once it is granted.
	AtGrant bool `json:"completes_at_grant,omitempty"`
}

// OwnerTakeoverLine is an open request owner who lists above the channels.
type OwnerTakeoverLine struct {
	ID          int64   `json:"id"`
	BindingID   int64   `json:"binding_id"`
	Channel     string  `json:"channel"`
	Alias       string  `json:"alias"`
	Vendor      string  `json:"vendor"`
	Session     string  `json:"session"`
	EndedAtMS   int64   `json:"ended_at_ms"`
	Verified    bool    `json:"end_verified"`
	LastSeenMS  int64   `json:"last_seen_at_ms,omitempty"`
	Owes        []int64 `json:"owes,omitempty"`
	AskedAtMS   int64   `json:"asked_at_ms"`
	Granted     bool    `json:"granted,omitempty"`
	ExpiresAtMS int64   `json:"expires_at_ms,omitempty"`
}

// takeoverGrantLife bounds how long a granted request waits for a session
// that was never enrolled to run join --takeover again.
const takeoverGrantLife = time.Hour

const (
	eventTakeoverRequested = "takeover_requested"
	eventTakeoverGranted   = "takeover_granted"
)

// takeoverAsk is the detail of a takeover_requested event. An enrolled
// requester is named by its principal; a session that never joined only by
// a hash of its key, so the log holds no credential.
type takeoverAsk struct {
	BindingID int64  `json:"binding_id"`
	Channel   string `json:"channel"`
	Alias     string `json:"alias"`
	Vendor    string `json:"vendor"`
	Principal int64  `json:"principal,omitempty"`
	KeyHash   string `json:"key_hash,omitempty"`
	Session   string `json:"session"`
}

type takeoverGrant struct {
	Request   int64 `json:"request"`
	BindingID int64 `json:"binding_id"`
	Completed bool  `json:"completed"`
}

func takeoverKeyHash(vendor, key string) string {
	sum := sha256.Sum256([]byte("takeover\x00" + vendor + ":" + key))
	return hex.EncodeToString(sum[:])
}

// sessionLabel is the short digest crosshall whoami shows for a session.
func sessionLabel(vendor, key string) string {
	sum := sha256.Sum256([]byte(vendor + "\x00" + key))
	return hex.EncodeToString(sum[:])[:12]
}

func bindingSubject(id int64) string { return "binding:" + strconv.FormatInt(id, 10) }

// sessionEnd is how a live binding's session ended: its route was closed by
// the session's end or its launch's, and nothing was seen from it since.
type sessionEnd struct {
	atMS     int64
	verified bool // the dispatcher saw the launch end, not only the session's word
}

// endedSession reads the end of binding's session from its route and the
// principal's observations only: the refused join that offers a takeover
// reads it, and must stay off the event log.
func endedSession(ctx context.Context, tx store.Tx, binding, principal int64) (sessionEnd, bool, error) {
	var end sessionEnd
	err := tx.QueryRowContext(ctx, `SELECT r.updated_at_ms, r.reason = 'launch ended' FROM publish_route r
		WHERE r.binding_id = ? AND r.state = 'ended' AND r.reason IN (?, 'launch ended')
			AND NOT EXISTS (SELECT 1 FROM observation o WHERE o.principal_id = ?
				AND o.observed_at_ms >= r.updated_at_ms AND o.kind <> 'session_ended')`,
		binding, routeSessionEnded, principal).Scan(&end.atMS, &end.verified)
	if errors.Is(err, sql.ErrNoRows) {
		return end, false, nil
	}
	return end, err == nil, err
}

// takenBinding is the live binding that holds an alias in a channel.
type takenBinding struct {
	id, principal int64
	vendor        string
}

func liveAliasBinding(ctx context.Context, tx store.Tx, channel int64, alias string) (takenBinding, bool, error) {
	var b takenBinding
	err := tx.QueryRowContext(ctx, `SELECT b.id, b.principal_id, p.vendor FROM binding b JOIN principal p ON p.id = b.principal_id
		WHERE b.channel_id = ? AND b.alias = ? AND b.state = 'live'`, channel, alias).Scan(&b.id, &b.principal, &b.vendor)
	if errors.Is(err, sql.ErrNoRows) {
		return b, false, nil
	}
	return b, err == nil, err
}

// takeoverOffer is the sentence an alias_taken refusal adds when the alias
// is held by a member of the caller's vendor whose session ended.
func takeoverOffer(ctx context.Context, tx store.Tx, channel channelRow, alias, vendor string) (string, error) {
	b, found, err := liveAliasBinding(ctx, tx, channel.id, alias)
	if err != nil || !found || b.vendor != vendor {
		return "", err
	}
	end, ended, err := endedSession(ctx, tx, b.id, b.principal)
	if err != nil || !ended {
		return "", err
	}
	word := "ended"
	if !end.verified {
		word = "was reported ended" // only by the session's own hook
	}
	return fmt.Sprintf(" @%s's last session %s %s. To ask the owner to give @%s to this session, run crosshall join %s --as %s --takeover.",
		alias, word, time.UnixMilli(end.atMS).Format("15:04"), alias, channel.name, alias), nil
}

// takeoverEvent is one takeover event of a binding.
type takeoverEvent struct {
	seq, atMS int64
	kind      string
	ask       takeoverAsk
	grant     takeoverGrant
}

// takeoverState is what a binding's takeover events say: its newest
// request, the grant of that request if any, and whether a restore or a
// departure of the binding since closed it.
type takeoverState struct {
	request *takeoverEvent
	grant   *takeoverEvent
	closed  bool
}

// readTakeover scans the event log for binding's takeover. The log has no
// index but its sequence, so only owner commands and an explicit join
// --takeover read it; no hook does.
func readTakeover(ctx context.Context, tx store.Tx, binding int64) (takeoverState, error) {
	var st takeoverState
	rows, err := tx.QueryContext(ctx, `SELECT seq, kind, detail, at_ms FROM event WHERE subject = ?
		AND kind IN (?, ?, 'binding_restored', 'binding_departed') ORDER BY seq`,
		bindingSubject(binding), eventTakeoverRequested, eventTakeoverGranted)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var e takeoverEvent
		var detail string
		if err := rows.Scan(&e.seq, &e.kind, &detail, &e.atMS); err != nil {
			return st, err
		}
		switch e.kind {
		case eventTakeoverRequested:
			if err := json.Unmarshal([]byte(detail), &e.ask); err != nil {
				return st, err
			}
			st = takeoverState{request: &e}
		case eventTakeoverGranted:
			if err := json.Unmarshal([]byte(detail), &e.grant); err != nil {
				return st, err
			}
			if st.request != nil && e.grant.Request == st.request.seq {
				st.grant = &e
				st.closed = st.closed || e.grant.Completed
			}
		default:
			st.closed = st.request != nil
		}
	}
	return st, rows.Err()
}

// requestTakeover records a join --takeover: the caller asks the owner for
// the live binding that holds cmd.Alias, whose session ended. It enrolls
// nothing. A session the owner already granted it completes it here.
func (s *Service) requestTakeover(ctx context.Context, tx store.Tx, caller core.Caller, req Request, ch channelRow, cmd Join, now time.Time) (core.Result, int64, error) {
	retry := fmt.Sprintf("crosshall join %s --as %s --takeover", ch.name, cmd.Alias)
	if cmd.Alias == "" {
		return Fail(core.ExitUsage, "", "--takeover needs the alias to take over", "crosshall join <channel> --as <alias> --takeover"), 0, nil
	}
	b, found, err := liveAliasBinding(ctx, tx, ch.id, cmd.Alias)
	if err != nil {
		return core.Result{}, 0, err
	}
	if !found {
		return Refuse(core.CodeNotFound, fmt.Sprintf("@%s is not a member of #%s", cmd.Alias, ch.name),
			fmt.Sprintf("crosshall join %s --as %s", ch.name, cmd.Alias)), 0, nil
	}
	var principal int64
	if caller.PrincipalID != nil {
		principal = *caller.PrincipalID
	}
	if principal == b.principal {
		// Already this session's: the join goes on as a plain join.
		return core.Result{}, principal, nil
	}
	if b.vendor != req.Vendor {
		return Refuse(core.CodeAliasTaken, fmt.Sprintf("@%s in #%s is a %s member", cmd.Alias, ch.name, b.vendor),
			"only a session of the same agent can take it over; crosshall join "+ch.name+" chooses an unused alias"), 0, nil
	}
	if req.SessionKey == "" {
		return core.Result{}, 0, errors.New("domain: takeover request without a session key")
	}
	st, err := readTakeover(ctx, tx, b.id)
	if err != nil {
		return core.Result{}, 0, err
	}
	hash := takeoverKeyHash(req.Vendor, req.SessionKey)
	// An expired grant closes its request: this run asks again.
	st.closed = st.closed || st.grant != nil && ms(now) > st.grant.atMS+takeoverGrantLife.Milliseconds()
	if principal == 0 && st.request != nil && !st.closed && st.request.ask.KeyHash == hash && st.grant != nil {
		return s.completeTakeover(ctx, tx, req, b, st.request.seq, now)
	}
	_, ended, err := endedSession(ctx, tx, b.id, b.principal)
	if err != nil {
		return core.Result{}, 0, err
	}
	if !ended {
		return Refuse(core.CodeAliasTaken, fmt.Sprintf("@%s's session in #%s has not ended", cmd.Alias, ch.name),
			"choose an unused alias: crosshall join "+ch.name), 0, nil
	}
	if principal != 0 {
		open, err := memberOpenRequests(ctx, tx, principal)
		if err != nil {
			return core.Result{}, 0, err
		}
		if len(open) > 0 {
			return Refuse(core.CodeBindingChanged, fmt.Sprintf("this session has open requests (#%d); taking over @%s would leave them", open[0], cmd.Alias),
				"reply to or cancel them first, then run "+retry), 0, nil
		}
	}
	out := TakeoverRequest{Channel: ch.name, Alias: cmd.Alias, Session: sessionLabel(req.Vendor, req.SessionKey), AtGrant: principal != 0}
	same := st.request != nil && !st.closed &&
		(principal != 0 && st.request.ask.Principal == principal || principal == 0 && st.request.ask.KeyHash == hash)
	if same {
		out.ID = st.request.seq
	} else {
		ask := takeoverAsk{BindingID: b.id, Channel: ch.name, Alias: cmd.Alias, Vendor: req.Vendor, Principal: principal, Session: out.Session}
		if principal == 0 {
			ask.KeyHash = hash
		}
		if out.ID, err = auditSeq(ctx, tx, eventTakeoverRequested, bindingSubject(b.id), principal, ask, now); err != nil {
			return core.Result{}, 0, err
		}
	}
	r, err := OK(JoinResult{Channel: ch.name, Topic: ch.topic, Alias: cmd.Alias, Takeover: &out})
	return r, 0, err
}

// auditSeq records an event as audit does, with no actor for a session
// that never joined, and returns its sequence.
func auditSeq(ctx context.Context, tx store.Tx, kind, subject string, actor int64, detail any, now time.Time) (int64, error) {
	raw, err := json.Marshal(detail)
	if err != nil {
		return 0, err
	}
	var by any
	if actor != 0 {
		by = actor
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO event (kind, subject, actor_id, detail, at_ms) VALUES (?, ?, ?, ?, ?)`,
		kind, subject, by, string(raw), ms(now))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// completeTakeover attaches a never-enrolled session's key to the binding
// the owner granted it, the way owner restore does.
func (s *Service) completeTakeover(ctx context.Context, tx store.Tx, req Request, b takenBinding, request int64, now time.Time) (core.Result, int64, error) {
	if _, ended, err := endedSession(ctx, tx, b.id, b.principal); err != nil || !ended {
		if err != nil {
			return core.Result{}, 0, err
		}
		return Refuse(core.CodeBindingChanged, fmt.Sprintf("takeover %d no longer applies: the member's session is active again", request), "crosshall join <channel> chooses an unused alias"), 0, nil
	}
	if s.RestoreKey == nil {
		return core.Result{}, 0, errors.New("domain: missing identity restore handler")
	}
	attached, err := s.RestoreKey(ctx, tx, b.principal, req.Vendor, req.SessionKey, now)
	if err != nil {
		return core.Result{}, 0, err
	}
	if !attached {
		return Refuse(core.CodeKeyInUse, "this session's key was already enrolled", "ask the owner to run crosshall owner restore"), 0, nil
	}
	if err := handOverBinding(ctx, tx, b.id, b.principal, b.principal, handedOver{BindingID: b.id, Takeover: request}, now); err != nil {
		return core.Result{}, 0, err
	}
	return core.Result{}, b.principal, nil
}

// handedOver is the detail of a binding_restored event; a takeover names
// its request, so the old conversation can be told who has the member.
type handedOver struct {
	BindingID int64 `json:"binding_id"`
	Takeover  int64 `json:"takeover,omitempty"`
}

// handOverBinding is the half of a restore that every hand-over shares: the
// binding's route retires and the old conversation's launches end. Those
// of its processes are fenced, so a /clear there cannot take the member
// back. A session-scoped launch (Codex) names no process, and the new
// session's start reopens that same row, so it only ends: fenced, it
// would keep the member from every notice.
func handOverBinding(ctx context.Context, tx store.Tx, binding, principal, actor int64, detail any, now time.Time) error {
	if err := retirePublishRoute(ctx, tx, binding, "binding restored", now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE adapter_launch SET fenced = fenced OR launch_ref <> ?,
		ended_hook_at_ms = COALESCE(ended_hook_at_ms, ?) WHERE principal_id = ?`, SessionLaunch, ms(now), principal); err != nil {
		return err
	}
	// The old conversation's process no longer hosts the member: if it
	// exited before the new session's first command, it would depart the
	// member by process. The next command records the new one.
	if err := forgetHost(ctx, tx, principal); err != nil {
		return err
	}
	return audit(ctx, tx, "binding_restored", bindingSubject(binding), actor, detail, now)
}

// ownerTakeover grants a takeover request. An enrolled requester's keys
// move to the binding's principal at once; a never-enrolled one completes
// it with its next join --takeover within takeoverGrantLife.
func (s *Service) ownerTakeover(ctx context.Context, tx store.Tx, caller core.Caller, req Request, cmd OwnerTakeover, now time.Time) (core.Result, error) {
	if caller.Role != core.RoleOwner {
		return Refuse(core.CodeOwnerOnly, "only the owner can give a member to another session", "run it from the owner's own shell, not an agent session"), nil
	}
	if cmd.ID <= 0 {
		return Fail(core.ExitUsage, "", "owner takeover needs a positive request ID", "crosshall owner who --all"), nil
	}
	owner, err := ownerPrincipal(ctx, tx, now)
	if err != nil {
		return core.Result{}, err
	}
	const scope = "owner:takeover"
	if r, found, err := replay(ctx, tx, owner, req.OpKey, scope, cmd); err != nil || found {
		return r, err
	}
	var subject string
	err = tx.QueryRowContext(ctx, `SELECT subject FROM event WHERE seq = ? AND kind = ?`, cmd.ID, eventTakeoverRequested).Scan(&subject)
	if errors.Is(err, sql.ErrNoRows) {
		return Refuse(core.CodeNotFound, fmt.Sprintf("no takeover request %d", cmd.ID), "crosshall owner who --all"), nil
	}
	if err != nil {
		return core.Result{}, err
	}
	var binding int64
	if _, err := fmt.Sscanf(subject, "binding:%d", &binding); err != nil {
		return core.Result{}, err
	}
	st, err := readTakeover(ctx, tx, binding)
	if err != nil {
		return core.Result{}, err
	}
	if st.request.seq != cmd.ID {
		return Refuse(core.CodeBindingChanged, fmt.Sprintf("takeover %d was replaced by takeover %d", cmd.ID, st.request.seq),
			fmt.Sprintf("crosshall owner takeover %d", st.request.seq)), nil
	}
	ask := st.request.ask
	if st.closed {
		return Refuse(core.CodeBindingChanged, fmt.Sprintf("takeover %d is closed: @%s's binding was restored, departed or already given", cmd.ID, ask.Alias), "crosshall owner who --all"), nil
	}
	b := takenBinding{id: binding}
	var state string
	if err := tx.QueryRowContext(ctx, `SELECT b.principal_id, b.state, p.vendor FROM binding b JOIN principal p ON p.id = b.principal_id
		WHERE b.id = ?`, binding).Scan(&b.principal, &state, &b.vendor); err != nil {
		return core.Result{}, err
	}
	if state != "live" {
		return Refuse(core.CodeBindingChanged, fmt.Sprintf("@%s in #%s is no longer a member", ask.Alias, ask.Channel), "crosshall owner who --all"), nil
	}
	if _, ended, err := endedSession(ctx, tx, b.id, b.principal); err != nil || !ended {
		if err != nil {
			return core.Result{}, err
		}
		return Refuse(core.CodeBindingChanged, fmt.Sprintf("@%s's session in #%s is active again", ask.Alias, ask.Channel), "crosshall owner who --all"), nil
	}
	out := OwnerTakeoverResult{ID: cmd.ID, BindingID: binding, Channel: ask.Channel, Alias: ask.Alias, Session: ask.Session}
	if st.grant != nil {
		out.ExpiresMS = st.grant.atMS + takeoverGrantLife.Milliseconds()
		if ms(now) > out.ExpiresMS {
			return Refuse(core.CodeBindingChanged, fmt.Sprintf("the grant of takeover %d expired", cmd.ID), "the session asks again with crosshall join --takeover"), nil
		}
		return Refuse(core.CodeBindingChanged, fmt.Sprintf("takeover %d is already granted", cmd.ID),
			fmt.Sprintf("the session becomes @%s when it runs crosshall join %s --as %s --takeover again", ask.Alias, ask.Channel, ask.Alias)), nil
	}
	if ask.Principal != 0 {
		refusal, err := s.moveSession(ctx, tx, owner, cmd.ID, ask, b, now)
		if err != nil || refusal != nil {
			return deref(refusal), err
		}
		out.Completed = true
	} else {
		out.ExpiresMS = ms(now) + takeoverGrantLife.Milliseconds()
	}
	if _, err := auditSeq(ctx, tx, eventTakeoverGranted, bindingSubject(binding), owner,
		takeoverGrant{Request: cmd.ID, BindingID: binding, Completed: out.Completed}, now); err != nil {
		return core.Result{}, err
	}
	r, err := OK(out)
	if err != nil {
		return core.Result{}, err
	}
	return r, record(ctx, tx, owner, req.OpKey, scope, cmd, r, now)
}

// moveSession gives an enrolled requester's session to the binding b: its
// live keys and leaves move to b's principal, whose own keys retire, and
// its own membership closes as a departure.
func (s *Service) moveSession(ctx context.Context, tx store.Tx, owner, request int64, ask takeoverAsk, b takenBinding, now time.Time) (*core.Result, error) {
	from := ask.Principal
	var live int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM principal_key WHERE principal_id = ? AND state = 'live' AND vendor = ?`,
		from, b.vendor).Scan(&live); err != nil {
		return nil, err
	}
	if live == 0 {
		r := Refuse(core.CodeBindingChanged, fmt.Sprintf("the session that asked for @%s is gone", ask.Alias), "crosshall owner who --all")
		return &r, nil
	}
	open, err := memberOpenRequests(ctx, tx, from)
	if err != nil {
		return nil, err
	}
	if len(open) > 0 {
		r := Refuse(core.CodeBindingChanged, fmt.Sprintf("the session that asked for @%s has open requests (#%d)", ask.Alias, open[0]),
			"it replies to or cancels them first, then asks again")
		return &r, nil
	}
	var own OwnerDepartResult
	var channel int64
	var vendor string
	err = tx.QueryRowContext(ctx, `SELECT b.id, b.channel_id, c.name, b.alias, p.vendor FROM member_selection s
		JOIN binding b ON b.id = s.binding_id JOIN channel c ON c.id = b.channel_id JOIN principal p ON p.id = b.principal_id
		WHERE s.principal_id = ? AND b.state = 'live'`, from).Scan(&own.BindingID, &channel, &own.Channel, &own.Alias, &vendor)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil {
		if err := departByOwner(ctx, tx, owner, channel, from, vendor, own, now); err != nil {
			return nil, err
		}
	}
	// The binding's old conversation is no longer the member's.
	if _, err := tx.ExecContext(ctx, `UPDATE principal_key SET state = 'retired', retired_at_ms = ?
		WHERE principal_id = ? AND state = 'live'`, ms(now), b.principal); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE leaf SET state = 'fenced' WHERE state = 'live' AND (vendor, key) IN
		(SELECT vendor, key FROM principal_key WHERE principal_id = ? AND state = 'retired')`, b.principal); err != nil {
		return nil, err
	}
	// A leaf references its key with the principal, so it lets go of the
	// principal while the key moves.
	if _, err := tx.ExecContext(ctx, `UPDATE leaf SET principal_id = NULL WHERE principal_id = ?1 AND (vendor, key) IN
		(SELECT vendor, key FROM principal_key WHERE principal_id = ?1 AND state = 'live')`, from); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE principal_key SET principal_id = ? WHERE principal_id = ? AND state = 'live'`, b.principal, from); err != nil {
		return nil, err
	}
	// The member's own keys retired above, so its live keys are the moved ones.
	if _, err := tx.ExecContext(ctx, `UPDATE leaf SET principal_id = ?1 WHERE principal_id IS NULL AND (vendor, key) IN
		(SELECT vendor, key FROM principal_key WHERE principal_id = ?1 AND state = 'live')`, b.principal); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE principal SET vendor = (SELECT vendor FROM principal WHERE id = ?1),
		vendor_version = (SELECT vendor_version FROM principal WHERE id = ?1) WHERE id = ?2`, from, b.principal); err != nil {
		return nil, err
	}
	if err := handOverBinding(ctx, tx, b.id, b.principal, owner, handedOver{BindingID: b.id, Takeover: request}, now); err != nil {
		return nil, err
	}
	// The asking session runs on and gets no new start: its open launches
	// and its route go with it, as a channel move carries a route.
	if err := carryLaunches(ctx, tx, from, b.principal, now); err != nil {
		return nil, err
	}
	if own.BindingID == 0 {
		return nil, nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE publish_route_proof SET activated_at_ms = NULL WHERE binding_id = ?`, b.id); err != nil {
		return nil, err
	}
	if err := registerMovedPublishRoute(ctx, tx, own.BindingID, b.id, now); err != nil {
		return nil, err
	}
	_, err = MarkReachChanges(ctx, tx, now)
	return nil, err
}

// carryLaunches gives principal to the open launches of principal from: a
// launch of the same reference (a session-scoped one) is reopened with the
// moving launch's values, and from's own launch ends.
func carryLaunches(ctx context.Context, tx store.Tx, from, to int64, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO adapter_launch (principal_id, launch_ref, profile, verified, started_hook_at_ms,
			process_pid, process_start_identity, ended_by_process, pid_namespace)
		SELECT ?1, launch_ref, profile, verified, started_hook_at_ms, process_pid, process_start_identity, ended_by_process, pid_namespace
		FROM adapter_launch WHERE principal_id = ?2 AND ended_hook_at_ms IS NULL AND fenced = 0
		ON CONFLICT(principal_id, launch_ref) DO UPDATE SET profile = excluded.profile, verified = excluded.verified,
			started_hook_at_ms = excluded.started_hook_at_ms, ended_hook_at_ms = NULL, fenced = 0,
			process_pid = excluded.process_pid, process_start_identity = excluded.process_start_identity,
			ended_by_process = excluded.ended_by_process, pid_namespace = excluded.pid_namespace`, to, from); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE adapter_launch SET ended_hook_at_ms = MAX(started_hook_at_ms, ?)
		WHERE principal_id = ? AND ended_hook_at_ms IS NULL AND fenced = 0`, ms(now), from)
	return err
}

// openTakeovers lists the requests owner who shows: the newest open
// request of each live binding, until its grant expires.
func openTakeovers(ctx context.Context, tx store.Tx, now time.Time) ([]OwnerTakeoverLine, error) {
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT subject FROM event WHERE kind = ?`, eventTakeoverRequested)
	if err != nil {
		return nil, err
	}
	var bindings []int64
	for rows.Next() {
		var subject string
		var id int64
		if err := rows.Scan(&subject); err != nil {
			rows.Close()
			return nil, err
		}
		if _, err := fmt.Sscanf(subject, "binding:%d", &id); err == nil {
			bindings = append(bindings, id)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	var out []OwnerTakeoverLine
	for _, binding := range bindings {
		st, err := readTakeover(ctx, tx, binding)
		if err != nil {
			return nil, err
		}
		if st.request == nil || st.closed {
			continue
		}
		line := OwnerTakeoverLine{ID: st.request.seq, BindingID: binding, Channel: st.request.ask.Channel, Alias: st.request.ask.Alias,
			Vendor: st.request.ask.Vendor, Session: st.request.ask.Session, AskedAtMS: st.request.atMS}
		if st.grant != nil {
			line.Granted, line.ExpiresAtMS = true, st.grant.atMS+takeoverGrantLife.Milliseconds()
			if ms(now) > line.ExpiresAtMS {
				continue
			}
		}
		var principal int64
		var state string
		if err := tx.QueryRowContext(ctx, `SELECT principal_id, state FROM binding WHERE id = ?`, binding).Scan(&principal, &state); err != nil {
			return nil, err
		}
		end, ended, err := endedSession(ctx, tx, binding, principal)
		if err != nil {
			return nil, err
		}
		if state != "live" || !ended {
			continue
		}
		line.EndedAtMS, line.Verified = end.atMS, end.verified
		var seen sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MAX(observed_at_ms) FROM observation WHERE principal_id = ? AND kind <> 'session_ended'`,
			principal).Scan(&seen); err != nil {
			return nil, err
		}
		line.LastSeenMS = seen.Int64
		if line.Owes, err = memberOpenRequests(ctx, tx, principal); err != nil {
			return nil, err
		}
		out = append(out, line)
	}
	return out, nil
}

// TakeoverGivenAway names the takeover that gave the member of the retired
// key behind a leaf digest to another session, for an explicit command
// from the old conversation. Hooks never ask: it scans the event log.
func TakeoverGivenAway(ctx context.Context, tx store.Tx, digest string) (string, error) {
	var binding, retiredAt int64
	var alias string
	err := tx.QueryRowContext(ctx, `SELECT b.id, b.alias, k.retired_at_ms FROM leaf l
		JOIN principal_key k ON k.vendor = l.vendor AND k.key = l.key
		JOIN member_selection s ON s.principal_id = k.principal_id JOIN binding b ON b.id = s.binding_id
		WHERE l.digest = ? AND k.state = 'retired' AND k.retired_by IS NULL`, digest).Scan(&binding, &alias, &retiredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var at int64
	var detail string
	err = tx.QueryRowContext(ctx, `SELECT detail, at_ms FROM event WHERE subject = ? AND kind = 'binding_restored'
		AND at_ms >= ? ORDER BY seq LIMIT 1`, bindingSubject(binding), retiredAt).Scan(&detail, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var by handedOver
	if json.Unmarshal([]byte(detail), &by) != nil || by.Takeover == 0 {
		return "", nil
	}
	return fmt.Sprintf("@%s was given to another session (takeover %d) at %s", alias, by.Takeover, time.UnixMilli(at).Format("15:04")), nil
}
