package domain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/notice"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// Hook verbs: what an installed vendor hook reports through `crosshall hook`.
const (
	VerbHookEnroll = wire.VerbHookEnroll
	VerbHookEvent  = wire.VerbHookEvent
)

// Facts a hook event can establish. Each records only what it observed.
const (
	FactPromptSubmitted   = wire.HookPromptSubmitted
	FactTurnFinishing     = wire.HookTurnFinishing
	FactApprovalRequested = wire.HookApprovalRequested
	FactSessionEnded      = wire.HookSessionEnded
)

// factCapMS is how long a fact stays current; both vendors use the same caps.
// A session end stays until the next start.
var factCapMS = map[string]int64{
	FactPromptSubmitted:   30 * 60_000,
	FactTurnFinishing:     2 * 3_600_000,
	FactApprovalRequested: 30 * 60_000,
}

// SessionLaunch is the launch reference of a manifest without a launch ID; a
// clean end there is an observation only.
const SessionLaunch = "session"

// OfferCap is the number of IDs one notice line carries.
const OfferCap = notice.OfferCap

// HookEnroll is a session start: restore, re-join after a clean end, or auto-join.
type HookEnroll struct{ wire.HookEnrollArgs }

// HookEvent is one observed fact of a launch.
type HookEvent struct{ wire.HookEventArgs }

func (HookEnroll) command()     {}
func (HookEnroll) Verb() string { return VerbHookEnroll }
func (HookEvent) command()      {}
func (HookEvent) Verb() string  { return VerbHookEvent }

func profileJSON(p wire.HookProfile) string {
	raw, _ := json.Marshal(p)
	return string(raw)
}

func evidence(p wire.HookProfile) string { return "adapter:" + p.Adapter + "@" + p.VendorVersion }

func validRef(ref string) bool {
	return len(ref) <= 128 && ValidText("reference", ref) == "" && !strings.ContainsAny(ref, "\n\r")
}

func validProfile(p wire.HookProfile) bool {
	for _, v := range []string{p.Adapter, p.ManifestRev, p.VendorVersion, p.Platform, p.Mode} {
		if v == "" || !validRef(v) {
			return false
		}
	}
	return ValidName(p.Adapter)
}

func hookUsage(msg string) core.Result { return Fail(core.ExitUsage, "", msg, "") }

// policyOpen reports whether adapter effects are on for this epoch. Off or a
// changed epoch means the hook prints nothing.
func policyOpen(ctx context.Context, tx store.Tx, adapter string, epoch uint64) (bool, error) {
	var enabled bool
	var current uint64
	err := tx.QueryRowContext(ctx, `SELECT enabled, epoch FROM adapter_policy WHERE adapter = ?`, adapter).
		Scan(&enabled, &current)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil // a missing policy means off
	}
	return err == nil && enabled && current == epoch, err
}

type hookBinding struct {
	id, channel    int64
	routeEpoch     int64
	joinedAtMS     int64
	alias, state   string
	channelName    string
	dnd            bool
	byHook         bool // departed by a clean session end, so a start restores it
	ownerHookFence sql.NullInt64
}

func bindingOf(ctx context.Context, tx store.Tx, principal int64) (hookBinding, bool, error) {
	var b hookBinding
	err := tx.QueryRowContext(ctx, `SELECT b.id, b.channel_id, b.route_epoch, b.joined_at_ms, b.alias, b.state, c.name, b.dnd, b.departed_by_hook, b.owner_hook_fence_ms
		FROM binding b JOIN channel c ON c.id = b.channel_id WHERE b.principal_id = ? ORDER BY b.id LIMIT 1`, principal).
		Scan(&b.id, &b.channel, &b.routeEpoch, &b.joinedAtMS, &b.alias, &b.state, &b.channelName, &b.dnd, &b.byHook, &b.ownerHookFence)
	if errors.Is(err, sql.ErrNoRows) {
		return b, false, nil
	}
	return b, err == nil, err
}

func hookLaunchAfterFence(ctx context.Context, tx store.Tx, principal int64, launch string, fence, at int64) (bool, error) {
	var fresh bool
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM adapter_launch WHERE principal_id = ? AND launch_ref = ?
			AND started_hook_at_ms > ? AND started_hook_at_ms <= ?)`,
		principal, launch, fence, at).Scan(&fresh)
	return fresh, err
}

// --- enroll ---------------------------------------------------------------

func (s *Service) hookEnroll(ctx context.Context, tx store.Tx, caller core.Caller, req Request, cmd HookEnroll, now time.Time) (core.Result, error) {
	if !validProfile(cmd.Profile) || cmd.LaunchRef == "" || !validRef(cmd.LaunchRef) || !validRef(cmd.EventRef) ||
		!ValidName(cmd.Source) || cmd.HookAtMS <= 0 || (cmd.RepoRoot != "" && !validRepoRoot(cmd.RepoRoot)) {
		return hookUsage("invalid hook.enroll arguments"), nil
	}
	if caller.Role == core.RoleOwner {
		return Refuse(core.CodeOwnerOnly, "hook verbs are for member sessions", ""), nil
	}
	out := wire.HookResult{Epoch: cmd.Epoch}
	if open, err := policyOpen(ctx, tx, cmd.Profile.Adapter, cmd.Epoch); err != nil || !open {
		return okOrErr(out, err)
	}
	// A start from a hook clock far ahead of the dispatcher's changes
	// nothing, a first enrollment included.
	if cmd.HookAtMS > ms(now)+futureSlackMS {
		return OK(out)
	}
	if caller.PrincipalID == nil {
		return s.hookAutoJoin(ctx, tx, req, cmd, 0, now)
	}
	principal := *caller.PrincipalID
	b, found, err := bindingOf(ctx, tx, principal)
	if err != nil {
		return core.Result{}, err
	}
	if found && b.ownerHookFence.Valid {
		if !b.byHook && b.state != "live" || cmd.HookAtMS <= b.ownerHookFence.Int64 {
			return OK(out)
		}
		if cmd.Source == "compact" {
			// Compaction can refresh a fresh launch but cannot establish one.
			fresh, err := hookLaunchAfterFence(ctx, tx, principal, cmd.LaunchRef, b.ownerHookFence.Int64, cmd.HookAtMS)
			if err != nil || !fresh {
				return okOrErr(out, err)
			}
		}
	}
	if claimed, err := claimHook(ctx, tx, principal, req.OpKey, cmd, now); err != nil || !claimed {
		return okOrErr(out, err) // a replay of a claimed callback prints nothing
	}
	if !found {
		return s.hookAutoJoin(ctx, tx, req, cmd, principal, now)
	}
	// A repeated start of the same launch at the same time is a replay: no text.
	var started int64
	err = tx.QueryRowContext(ctx, `SELECT started_hook_at_ms FROM adapter_launch WHERE principal_id = ? AND launch_ref = ?`,
		principal, cmd.LaunchRef).Scan(&started)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return core.Result{}, err
	}
	if err == nil && started == cmd.HookAtMS {
		return OK(out)
	}
	if b.state != "live" {
		var lastEnd sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT MAX(ended_hook_at_ms) FROM adapter_launch WHERE principal_id = ?`,
			principal).Scan(&lastEnd); err != nil {
			return core.Result{}, err
		}
		// A member who left by hand is not brought back by a hook, and a
		// compaction or any start older than the clean end is a stale callback.
		if !b.byHook || cmd.Source == "compact" || (lastEnd.Valid && cmd.HookAtMS <= lastEnd.Int64) {
			out.Enrollment = "none"
			return OK(out)
		}
		if b.alias, err = rejoin(ctx, tx, b, principal, req.Vendor, now); err != nil {
			return core.Result{}, err
		}
	}
	before, err := presenceOf(ctx, tx, principal, now)
	if err != nil {
		return core.Result{}, err
	}
	if err := registerLaunch(ctx, tx, principal, cmd.Profile, cmd.LaunchRef, cmd.LaunchVerified, cmd.HookAtMS); err != nil {
		return core.Result{}, err
	}
	if _, _, err := s.settleLaunches(ctx, tx, principal, cmd.LaunchRef, cmd.HookAtMS, now); err != nil {
		return core.Result{}, err
	}
	if err := registerPublishRouteFromEnroll(ctx, tx, b.id, b.routeEpoch, b.joinedAtMS, principal, req.SessionKey, cmd, now); err != nil {
		return core.Result{}, err
	}
	// The launch and the live binding are written: project the member's capabilities.
	if err := refreshMemberAdapterCapabilities(ctx, tx, principal, cmd.Profile.Adapter); err != nil {
		return core.Result{}, err
	}
	if err := presenceChanged(ctx, tx, b.channel, principal, before, now); err != nil {
		return core.Result{}, err
	}
	out.Enrollment, out.Alias, out.Channel = "restored", b.alias, b.channelName
	if err := offerPending(ctx, tx, principal, b, cmd.Profile, cmd.LaunchRef, cmd.Epoch, "start", &out, now); err != nil {
		return core.Result{}, err
	}
	if cmd.Source == "clear" || cmd.Source == "compact" {
		if out.Cue, err = resetCue(ctx, tx, principal, b, cmd.Source, out.PendingLine); err != nil {
			return core.Result{}, err
		}
	}
	return OK(out)
}

// resetCue is what a session gets back after its context was reset. Nothing
// already offered is listed again: a cleared session is told who it is and
// where the rules are, and work that this start does not offer is one count.
func resetCue(ctx context.Context, tx store.Tx, principal int64, b hookBinding, source, offered string) (string, error) {
	var lines []string
	if source == "clear" {
		lines = append(lines, ResetHint(b.alias, b.channelName))
	}
	if !b.dnd {
		summary, err := memberInboxSummary(ctx, tx, principal, b.channel)
		if err != nil {
			return "", err
		}
		shown := len(strings.Fields(strings.TrimPrefix(offered, "[crosshall] pending: ")))
		if len(summary.Pending) > shown || summary.OwedCount > 0 {
			lines = append(lines, "[crosshall] "+strconv.Itoa(len(summary.Pending))+" unread, "+strconv.Itoa(summary.OwedCount)+
				" owed. Run crosshall inbox.")
		}
	}
	return strings.Join(lines, "\n"), nil
}

// hookAutoJoin joins the repository's default channel: generated alias, zero tokens.
func (s *Service) hookAutoJoin(ctx context.Context, tx store.Tx, req Request, cmd HookEnroll, principal int64, now time.Time) (core.Result, error) {
	out := wire.HookResult{Enrollment: "none", Epoch: cmd.Epoch}
	if cmd.RepoRoot == "" {
		return OK(out)
	}
	ch, found, err := defaultChannel(ctx, tx, cmd.RepoRoot)
	if err != nil || !found || ch.state != "active" {
		return okOrErr(out, err)
	}
	if principal == 0 {
		if req.Enroll == nil {
			return core.Result{}, errors.New("domain: hook enrollment without an enrollment link")
		}
		vendor := req.Vendor
		if vendor == "" {
			vendor = "unknown"
		}
		res, err := tx.ExecContext(ctx,
			`INSERT INTO principal (kind, vendor, root_tokens, created_at_ms) VALUES ('session', ?, 0, ?)`, vendor, ms(now))
		if err != nil {
			return core.Result{}, err
		}
		if principal, err = res.LastInsertId(); err != nil {
			return core.Result{}, err
		}
		if err := req.Enroll(ctx, tx, principal); err != nil {
			return core.Result{}, err
		}
		if _, err := claimHook(ctx, tx, principal, req.OpKey, cmd, now); err != nil {
			return core.Result{}, err
		}
	}
	alias, err := generateAlias(ctx, tx, ch.id, req.Vendor)
	if err != nil {
		return core.Result{}, err
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO binding (principal_id, channel_id, alias, state, joined_at_ms, focus, focus_at_ms) VALUES (?, ?, ?, 'live', ?, '', ?)`,
		principal, ch.id, alias, ms(now), ms(now))
	if err != nil {
		return core.Result{}, err
	}
	bindingID, err := res.LastInsertId()
	if err != nil {
		return core.Result{}, err
	}
	if err := drawJoinToken(ctx, tx, ch.id, principal, now); err != nil {
		return core.Result{}, err
	}
	if err := rosterChange(ctx, tx, ch.id, principal, "joined"); err != nil {
		return core.Result{}, err
	}
	if err := audit(ctx, tx, "joined", "channel:"+ch.name, principal, map[string]string{"alias": alias, "by": "hook"}, now); err != nil {
		return core.Result{}, err
	}
	if err := registerLaunch(ctx, tx, principal, cmd.Profile, cmd.LaunchRef, cmd.LaunchVerified, cmd.HookAtMS); err != nil {
		return core.Result{}, err
	}
	if _, _, err := s.settleLaunches(ctx, tx, principal, cmd.LaunchRef, cmd.HookAtMS, now); err != nil {
		return core.Result{}, err
	}
	if err := registerPublishRouteFromEnroll(ctx, tx, bindingID, 1, ms(now), principal, req.SessionKey, cmd, now); err != nil {
		return core.Result{}, err
	}
	if err := refreshMemberAdapterCapabilities(ctx, tx, principal, cmd.Profile.Adapter); err != nil {
		return core.Result{}, err
	}
	seq, err := rosterSeq(ctx, tx, ch.id)
	if err != nil {
		return core.Result{}, err
	}
	// The snapshot shown here is the baseline of the member's first ordinary command.
	if _, err := tx.ExecContext(ctx, `UPDATE binding SET roster_baseline_seq = ? WHERE id = ?`, seq, bindingID); err != nil {
		return core.Result{}, err
	}
	members, err := roster(ctx, tx, ch.id, principal, now)
	if err != nil {
		return core.Result{}, err
	}
	if len(members) > RosterCap {
		out.Omitted = len(members) - RosterCap
		members = members[:RosterCap]
	}
	if out.Roster, err = json.Marshal(members); err != nil {
		return core.Result{}, err
	}
	out.Enrollment, out.Alias, out.Channel, out.Hint = "joined", alias, ch.name, Hint(alias, ch.name)
	b := hookBinding{id: bindingID, channel: ch.id, alias: alias, state: "live", channelName: ch.name}
	if err := offerPending(ctx, tx, principal, b, cmd.Profile, cmd.LaunchRef, cmd.Epoch, "start", &out, now); err != nil {
		return core.Result{}, err
	}
	return OK(out)
}

func okOrErr(out wire.HookResult, err error) (core.Result, error) {
	if err != nil {
		return core.Result{}, err
	}
	return OK(out)
}

func registerLaunch(ctx context.Context, tx store.Tx, principal int64, p wire.HookProfile, launch string, verified bool, at int64) error {
	// A newer start of a known launch (a resumed session-scoped launch)
	// reactivates it; an older or equal one changes nothing.
	_, err := tx.ExecContext(ctx, `INSERT INTO adapter_launch (principal_id, launch_ref, profile, verified, started_hook_at_ms)
		VALUES (?, ?, ?, ?, ?) ON CONFLICT(principal_id, launch_ref) DO UPDATE SET
		started_hook_at_ms = excluded.started_hook_at_ms, ended_hook_at_ms = NULL,
		profile = excluded.profile, verified = excluded.verified
		WHERE excluded.started_hook_at_ms > adapter_launch.started_hook_at_ms`, principal, launch, profileJSON(p), verified, at)
	return err
}

// claimHook records a hook callback's operation key in the writer
// transaction. False means the callback was already claimed: a replay.
func claimHook(ctx context.Context, tx store.Tx, principal int64, key string, cmd Command, now time.Time) (bool, error) {
	if key == "" {
		return true, nil
	}
	var n int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM op WHERE principal_id = ? AND op_key = ?`, principal, key).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, nil
	}
	empty, err := OK(wire.HookResult{})
	if err != nil {
		return false, err
	}
	return true, record(ctx, tx, principal, key, "hook", cmd, empty, now)
}

// rejoin brings a binding departed by a clean end back, under its alias when
// free, else a generated one: a hook cannot ask for another.
func rejoin(ctx context.Context, tx store.Tx, b hookBinding, principal int64, vendor string, now time.Time) (string, error) {
	alias := b.alias
	taken, err := aliasLive(ctx, tx, b.channel, alias)
	if err != nil {
		return "", err
	}
	if taken {
		if alias, err = generateAlias(ctx, tx, b.channel, vendor); err != nil {
			return "", err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE binding SET state = 'live', departed_at_ms = NULL, departed_by_hook = 0,
		alias = ?, joined_at_ms = ? WHERE id = ?`, alias, ms(now), b.id); err != nil {
		return "", err
	}
	if err := rosterChange(ctx, tx, b.channel, principal, "joined"); err != nil {
		return "", err
	}
	return alias, audit(ctx, tx, "rejoined", "binding:"+strconv.FormatInt(b.id, 10), principal, map[string]string{"alias": alias}, now)
}

// --- events ---------------------------------------------------------------

func (s *Service) hookEvent(ctx context.Context, tx store.Tx, caller core.Caller, req Request, cmd HookEvent, now time.Time) (core.Result, error) {
	switch cmd.Fact {
	case FactPromptSubmitted, FactTurnFinishing, FactApprovalRequested, FactSessionEnded:
	default:
		return hookUsage("unknown hook fact"), nil
	}
	if !validProfile(cmd.Profile) || cmd.LaunchRef == "" || !validRef(cmd.LaunchRef) || cmd.EventRef == "" ||
		!validRef(cmd.EventRef) || !validRef(cmd.TurnRef) || cmd.HookAtMS <= 0 {
		return hookUsage("invalid hook.event arguments"), nil
	}
	out := wire.HookResult{Epoch: cmd.Epoch}
	if caller.Role != core.RoleMember || caller.PrincipalID == nil {
		return OK(out)
	}
	principal := *caller.PrincipalID
	if open, err := policyOpen(ctx, tx, cmd.Profile.Adapter, cmd.Epoch); err != nil || !open {
		return okOrErr(out, err)
	}
	b, found, err := bindingOf(ctx, tx, principal)
	if err != nil {
		return core.Result{}, err
	}
	if found && b.ownerHookFence.Valid {
		if b.state != "live" || cmd.HookAtMS <= b.ownerHookFence.Int64 {
			return OK(out)
		}
		fresh, err := hookLaunchAfterFence(ctx, tx, principal, cmd.LaunchRef, b.ownerHookFence.Int64, cmd.HookAtMS)
		if err != nil || !fresh {
			return okOrErr(out, err)
		}
	}
	origin := "unknown"
	if cmd.Receipt != "" && found && b.state == "live" && (caller.BindingID == nil || *caller.BindingID == b.id) {
		// An absorbed notice reuses the original prompt's operation key.
		ownTurn, err := observePublishReceipt(ctx, tx, b, principal, req.SessionKey, cmd, now)
		if err != nil {
			return core.Result{}, err
		}
		if ownTurn {
			origin = "crosshall_wake"
		}
	}
	if claimed, err := claimHook(ctx, tx, principal, req.OpKey, cmd, now); err != nil || !claimed {
		return okOrErr(out, err)
	}
	before, err := presenceOf(ctx, tx, principal, now)
	if err != nil {
		return core.Result{}, err
	}
	var fresh any
	if limit := factCapMS[cmd.Fact]; limit > 0 {
		fresh = cmd.HookAtMS + limit
	}
	// The (launch, event) pair claims the event: a second copy changes and prints nothing.
	res, err := tx.ExecContext(ctx, `INSERT INTO observation
		(principal_id, kind, origin, source, observed_at_ms, fresh_until_ms, launch_ref, event_ref, turn_ref, hook_at_ms, profile)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, NULLIF(?, ''), ?, ?)
		ON CONFLICT DO NOTHING`,
		principal, cmd.Fact, origin, evidence(cmd.Profile), ms(now), fresh, cmd.LaunchRef, cmd.EventRef, cmd.TurnRef, cmd.HookAtMS,
		profileJSON(cmd.Profile))
	if err != nil {
		return core.Result{}, err
	}
	if n, err := res.RowsAffected(); err != nil || n == 0 {
		return okOrErr(out, err)
	}
	// A hook clock far ahead of the dispatcher's: the fact is stored, not applied.
	if cmd.HookAtMS > ms(now)+futureSlackMS {
		return OK(out)
	}
	live := found && b.state == "live"
	// Launches that can no longer be current are ended first, so a killed
	// newer launch cannot hide a running older one (#100).
	otherAlive, settled, err := s.settleLaunches(ctx, tx, principal, cmd.LaunchRef, cmd.HookAtMS, now)
	if err != nil {
		return core.Result{}, err
	}
	if settled && live {
		if err := refreshMemberAdapterCapabilities(ctx, tx, principal, cmd.Profile.Adapter); err != nil {
			return core.Result{}, err
		}
	}
	// Only the latest started launch that has not ended counts (the latest one
	// when all have ended). An event of a launch whose start was never seen is
	// stored only: it cannot establish which launch is newest.
	latest, err := latestLaunch(ctx, tx, principal)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return core.Result{}, err
	}
	if err != nil || latest != cmd.LaunchRef {
		// The clean end of a launch that is not current ends that launch
		// only: it departs nobody, and it can no longer become current.
		if err == nil && cmd.Fact == FactSessionEnded && cmd.LaunchVerified && cmd.LaunchRef != SessionLaunch {
			if _, err := tx.ExecContext(ctx, `UPDATE adapter_launch SET ended_hook_at_ms = MAX(COALESCE(ended_hook_at_ms, 0), ?)
				WHERE principal_id = ? AND launch_ref = ? AND started_hook_at_ms <= ?`,
				cmd.HookAtMS, principal, cmd.LaunchRef, cmd.HookAtMS); err != nil {
				return core.Result{}, err
			}
		}
		// The event itself changes nothing, but settling may have made
		// another launch current, and with it the member's presence.
		if settled && live {
			return okOrErr(out, presenceChanged(ctx, tx, b.channel, principal, before, now))
		}
		return OK(out)
	}
	switch cmd.Fact {
	case FactPromptSubmitted:
		if cmd.TurnRef != "" {
			// A prompt older than a turn already opened is stale: it opens its
			// turn as superseded and supersedes nothing.
			var newer int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM turn WHERE principal_id = ? AND launch_ref = ?
				AND turn_ref <> ? AND opened_hook_at_ms > ?`, principal, cmd.LaunchRef, cmd.TurnRef, cmd.HookAtMS).Scan(&newer); err != nil {
				return core.Result{}, err
			}
			if newer == 0 {
				if _, err := tx.ExecContext(ctx, `UPDATE turn SET superseded = 1 WHERE principal_id = ? AND launch_ref = ? AND turn_ref <> ?`,
					principal, cmd.LaunchRef, cmd.TurnRef); err != nil {
					return core.Result{}, err
				}
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO turn (principal_id, launch_ref, turn_ref, opened_hook_at_ms, superseded)
				VALUES (?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, principal, cmd.LaunchRef, cmd.TurnRef, cmd.HookAtMS, newer > 0); err != nil {
				return core.Result{}, err
			}
			if newer > 0 {
				return OK(out) // a stale prompt is an observation: no offer, no acknowledgement
			}
		}
	case FactTurnFinishing:
		if _, err := tx.ExecContext(ctx, `UPDATE turn SET finishing_hook_at_ms = COALESCE(finishing_hook_at_ms, ?)
			WHERE principal_id = ? AND launch_ref = ? AND turn_ref = ?`, cmd.HookAtMS, principal, cmd.LaunchRef, cmd.TurnRef); err != nil {
			return core.Result{}, err
		}
	case FactSessionEnded:
		if live {
			sessionRef := resolveSessionKey(ctx, tx, req.SessionKey, principal, cmd.Profile.Adapter)
			if err := endPublishRoute(ctx, tx, b.id, b.routeEpoch, cmd.LaunchRef, sessionRef, cmd.Epoch, cmd.HookAtMS, "session ended", now); err != nil {
				return core.Result{}, err
			}
		}
		// Only a bound launch can end: an unverified (session-scoped) end is an
		// observation, and its launch stays eligible for the next notice. An
		// end older than its launch's start is a stale callback.
		if !cmd.LaunchVerified || cmd.LaunchRef == SessionLaunch {
			break
		}
		// The accepted end time only moves forward: a later-arriving older end
		// cannot lower the cutoff that a restore is checked against.
		res, err := tx.ExecContext(ctx, `UPDATE adapter_launch SET ended_hook_at_ms = MAX(COALESCE(ended_hook_at_ms, 0), ?)
			WHERE principal_id = ? AND launch_ref = ? AND started_hook_at_ms <= ?`,
			cmd.HookAtMS, principal, cmd.LaunchRef, cmd.HookAtMS)
		if err != nil {
			return core.Result{}, err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			return okOrErr(out, err)
		}
		// A clean session end departs the member only when the manifest bound the launch
		// reference (launch_verified, independent of version and mode); any
		// other end is an observation.
		if live && cmd.LaunchVerified && cmd.LaunchRef != SessionLaunch {
			// Another process of the same session (a resumed copy) keeps it,
			// and its launch is current now.
			if otherAlive {
				if err := refreshMemberAdapterCapabilities(ctx, tx, principal, cmd.Profile.Adapter); err != nil {
					return core.Result{}, err
				}
				break
			}
			if err := endSession(ctx, tx, b, principal, now); err != nil {
				return core.Result{}, err
			}
			// A member that left is no longer available through its adapter.
			return okOrErr(out, refreshMemberAdapterCapabilities(ctx, tx, principal, cmd.Profile.Adapter))
		}
	}
	if live {
		if err := presenceChanged(ctx, tx, b.channel, principal, before, now); err != nil {
			return core.Result{}, err
		}
	}
	if live && cmd.WantOutput {
		switch cmd.Fact {
		case FactPromptSubmitted:
			if err := offerPending(ctx, tx, principal, b, cmd.Profile, cmd.LaunchRef, cmd.Epoch, "prompt", &out, now); err != nil {
				return core.Result{}, err
			}
		case FactTurnFinishing:
			out.TurnEndLine, err = claimTurnEndNotice(ctx, tx, principal, b.channel, cmd, now)
			if err != nil {
				return core.Result{}, err
			}
		}
	}
	return OK(out)
}

func latestLaunch(ctx context.Context, tx store.Tx, principal int64) (string, error) {
	var ref string
	err := tx.QueryRowContext(ctx, `SELECT launch_ref FROM adapter_launch WHERE principal_id = ?
		ORDER BY ended_hook_at_ms IS NOT NULL, started_hook_at_ms DESC, rowid DESC LIMIT 1`, principal).Scan(&ref)
	return ref, err
}

// settleLaunches ends, at the time at, each open bound launch that can no
// longer be current beside launch: an earlier launch of the same process (a
// process runs one launch at a time, so its later start or end supersedes
// it) and a launch of another process that no longer runs (killed without
// an end). It reports whether a launch of another process still runs and
// whether it ended any launch. A launch whose start was never seen settles
// nothing.
func (s *Service) settleLaunches(ctx context.Context, tx store.Tx, principal int64, launch string, at int64, now time.Time) (alive, changed bool, err error) {
	if s.LaunchProcess == nil {
		return false, false, nil
	}
	own, ok := s.LaunchProcess(launch)
	if !ok {
		return false, false, nil
	}
	var started int64
	err = tx.QueryRowContext(ctx, `SELECT started_hook_at_ms FROM adapter_launch WHERE principal_id = ? AND launch_ref = ?`,
		principal, launch).Scan(&started)
	if errors.Is(err, sql.ErrNoRows) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT launch_ref, started_hook_at_ms FROM adapter_launch
		WHERE principal_id = ? AND launch_ref <> ? AND ended_hook_at_ms IS NULL AND started_hook_at_ms <= ?`,
		principal, launch, at)
	if err != nil {
		return false, false, err
	}
	var gone []string
	for rows.Next() {
		var ref string
		var began int64
		if err := rows.Scan(&ref, &began); err != nil {
			rows.Close()
			return false, false, err
		}
		process, ok := s.LaunchProcess(ref)
		switch {
		case !ok:
		case process == own:
			if began < started {
				gone = append(gone, ref)
			}
		case s.LaunchAlive == nil:
		case s.LaunchAlive(ref, began):
			alive = true
		default:
			gone = append(gone, ref)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return false, false, err
	}
	for _, ref := range gone {
		if _, err := tx.ExecContext(ctx, `UPDATE adapter_launch SET ended_hook_at_ms = ?
			WHERE principal_id = ? AND launch_ref = ? AND ended_hook_at_ms IS NULL`, at, principal, ref); err != nil {
			return false, false, err
		}
		// A publish route to an ended launch ends with it, as on its own end.
		if _, err := tx.ExecContext(ctx, `UPDATE publish_route
			SET state = 'ended', reason = 'launch ended', generation = generation + 1, updated_at_ms = ?
			WHERE principal_id = ? AND launch_ref = ? AND state <> 'ended'`, ms(now), principal, ref); err != nil {
			return false, false, err
		}
	}
	return alive, len(gone) > 0, nil
}

// endSession departs a live binding on the clean end of its latest launch,
// keeping the principal and its obligations; the next start restores it.
func endSession(ctx context.Context, tx store.Tx, b hookBinding, principal int64, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE binding SET state = 'departed', departed_at_ms = ?, departed_by_hook = 1 WHERE id = ?`,
		ms(now), b.id); err != nil {
		return err
	}
	if err := rosterChange(ctx, tx, b.channel, principal, "departed"); err != nil {
		return err
	}
	return audit(ctx, tx, "session_ended", "binding:"+strconv.FormatInt(b.id, 10), principal, map[string]string{"alias": b.alias}, now)
}

// --- offers -----------------------------------------------------------------

// offerPending commits an offer of the oldest unread items the session was
// not told about yet, by an earlier offer or by a turn-end notice, before any
// output. It asks for a notice acknowledgement only when nothing eligible
// remains.
func offerPending(ctx context.Context, tx store.Tx, principal int64, b hookBinding, p wire.HookProfile, launch string, epoch uint64,
	reason string, out *wire.HookResult, now time.Time) error {
	if b.dnd {
		return nil // held: no offer, nothing spent, no acknowledgement
	}
	ids, err := selectIDs(ctx, tx, `SELECT r.message_id FROM recipient r JOIN message m ON m.id = r.message_id
		WHERE r.principal_id = ? AND m.channel_id = ? AND r.picked_up_at_ms IS NULL AND `+notMoreTime+`
			AND `+noClosedObligation+`
			AND NOT EXISTS (SELECT 1 FROM offer_item oi WHERE oi.principal_id = r.principal_id AND oi.message_id = r.message_id)
			AND NOT EXISTS (SELECT 1 FROM announcement a WHERE a.principal_id = r.principal_id AND a.message_id = r.message_id
				AND a.via = 'turn_end')
		ORDER BY r.message_id LIMIT ?`, principal, b.channel, OfferCap+1)
	if err != nil {
		return err
	}
	remainder := len(ids) > OfferCap
	if remainder {
		ids = ids[:OfferCap]
	}
	if !remainder {
		out.Ack = &wire.HookAck{Kind: "notice"}
	}
	if len(ids) == 0 {
		return nil
	}
	var generation string
	if err := tx.QueryRowContext(ctx, `SELECT generation FROM leaf WHERE principal_id = ? AND state = 'live' ORDER BY created_at_ms DESC LIMIT 1`,
		principal).Scan(&generation); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO offer (principal_id, binding_id, generation, launch_ref, epoch, reason, evidence, at_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, principal, b.id, generation, launch, epoch, reason, evidence(p), ms(now))
	if err != nil {
		return err
	}
	if out.OfferID, err = res.LastInsertId(); err != nil {
		return err
	}
	parts := make([]string, len(ids))
	for i, id := range ids {
		if _, err := tx.ExecContext(ctx, `INSERT INTO offer_item (offer_id, message_id, principal_id) VALUES (?, ?, ?)`,
			out.OfferID, id, principal); err != nil {
			return err
		}
		parts[i] = strconv.FormatInt(id, 10)
	}
	out.PendingLine = "[crosshall] pending: " + strings.Join(parts, " ")
	return nil
}
