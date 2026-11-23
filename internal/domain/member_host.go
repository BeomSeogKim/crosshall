package domain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// A member's host is the agent process its commands named. A live member
// with no open launch is departed, as by a clean session end, when that
// process is proven gone; its next command from a live host brings it back.

// judgeableHost reports whether the dispatcher can later judge the process
// a request named: both fields are set and its PID is in the dispatcher's
// own namespace.
func (s *Service) judgeableHost(h *wire.Host) bool {
	own := ""
	if s.LaunchPIDNamespace != nil {
		own = s.LaunchPIDNamespace()
	}
	return h != nil && h.PID > 1 && h.PID <= math.MaxInt32 && h.Start > 0 && h.NS == own
}

// noteHost records the host a member command named. An unchanged host
// writes nothing; a new one, a session resumed in a new process, replaces
// the old one.
func (s *Service) noteHost(ctx context.Context, tx store.Tx, principal int64, h *wire.Host, now time.Time) error {
	if !s.judgeableHost(h) {
		return nil
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO member_host (principal_id, pid, start_identity, pid_namespace, seen_at_ms)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (principal_id) DO UPDATE SET pid = excluded.pid, start_identity = excluded.start_identity,
			pid_namespace = excluded.pid_namespace, seen_at_ms = excluded.seen_at_ms
		WHERE pid <> excluded.pid OR start_identity <> excluded.start_identity OR pid_namespace <> excluded.pid_namespace`,
		principal, h.PID, h.Start, h.NS, ms(now))
	return err
}

func forgetHost(ctx context.Context, tx store.Tx, principal int64) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM member_host WHERE principal_id = ?`, principal)
	return err
}

// returnByHost brings back a member that a clean session end departed (a
// hook's or a process check's) when its command names a host that runs:
// the session was resumed. A member that left by hand or was departed by
// the owner has departed_by_hook unset and stays away. It returns the alias
// when it differs from the old one, and updates the caller.
func (s *Service) returnByHost(ctx context.Context, tx store.Tx, caller *core.Caller, req Request, now time.Time) (string, error) {
	if caller.Role != core.RoleUnenrolled || caller.PrincipalID == nil || s.ProcessGone == nil || !s.judgeableHost(req.Host) {
		return "", nil
	}
	switch req.Command.(type) {
	case Join, HookEnroll, HookEvent, HookRoute:
		return "", nil
	}
	principal := *caller.PrincipalID
	b, found, err := bindingOf(ctx, tx, principal)
	if err != nil || !found || b.state != "departed" || !b.byHook {
		return "", err
	}
	var active bool
	if err := tx.QueryRowContext(ctx, `SELECT state = 'active' FROM channel WHERE id = ?`, b.channel).Scan(&active); err != nil || !active {
		return "", err
	}
	if s.ProcessGone(req.Host.PID, req.Host.Start) {
		return "", nil
	}
	alias, err := rejoin(ctx, tx, b, principal, req.Vendor, now)
	if err != nil {
		return "", err
	}
	membership := core.Membership{BindingID: b.id}
	if err := tx.QueryRowContext(ctx, `SELECT generation FROM member_selection WHERE principal_id = ? AND binding_id = ?`,
		principal, b.id).Scan(&membership.Generation); err != nil {
		return "", err
	}
	caller.Role, caller.BindingID, caller.Membership = core.RoleMember, &membership.BindingID, &membership
	if alias == b.alias {
		return "", nil
	}
	return alias, nil
}

// withReturnedAlias tells a member that came back under another alias.
func withReturnedAlias(result core.Result, alias string) (core.Result, error) {
	if alias == "" || result.Status != core.ExitOK {
		return result, nil
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(result.Data, &fields); err != nil {
		return core.Result{}, err
	}
	fields["rejoined_as"], _ = json.Marshal(alias)
	var err error
	result.Data, err = json.Marshal(fields)
	return result, err
}

// HostedMember is a live member without an open launch whose host the
// dispatcher can probe.
type HostedMember struct {
	PrincipalID, BindingID, BindingEpoch int64
	MembershipGeneration                 int64
	PID, StartIdentity                   int64
	PIDNamespace                         string
}

// HostedMembers lists, after a principal, the live members that have a host
// and no governing launch: a member whose launch the process sweep checks
// ends by that launch.
func HostedMembers(ctx context.Context, tx store.Tx, afterPrincipal int64, limit int) ([]HostedMember, error) {
	rows, err := tx.QueryContext(ctx, `SELECT h.principal_id, b.id, b.route_epoch, s.generation, h.pid, h.start_identity, h.pid_namespace
		FROM member_host h JOIN member_selection s ON s.principal_id = h.principal_id JOIN binding b ON b.id = s.binding_id
		WHERE h.principal_id > ? AND b.state = 'live' AND NOT `+governingLaunch+`
		ORDER BY h.principal_id LIMIT ?`, afterPrincipal, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var members []HostedMember
	for rows.Next() {
		var m HostedMember
		if err := rows.Scan(&m.PrincipalID, &m.BindingID, &m.BindingEpoch, &m.MembershipGeneration, &m.PID, &m.StartIdentity, &m.PIDNamespace); err != nil {
			return nil, err
		}
		members = append(members, m)
	}
	return members, rows.Err()
}

// governingLaunch is true when the principal has an open launch the process
// sweep checks: a verified Claude Code launch, whatever the adapter switch.
// Only such a launch keeps a member from its host. Any other open launch is
// ended by nothing certain: a session launch (Antigravity's, Codex's, or
// Claude Code's without a launch ID) ends by no hook, and an end hook does
// not fire on a kill, a crash or a closed terminal. Its member's host
// governs.
const governingLaunch = `EXISTS (SELECT 1 FROM adapter_launch l WHERE l.principal_id = h.principal_id
	AND l.ended_hook_at_ms IS NULL AND l.fenced = 0 AND l.verified = 1
	AND json_extract(l.profile, '$.adapter') = 'claude' AND l.launch_ref GLOB 'pid-*')`

// DepartGoneHosts departs each member whose probed host was proven gone,
// if nothing changed since the probe: the binding is the same live one, the
// host row still names the probed process, and no governing launch opened.
// Its open launches end with it, so a start older than the departure is a
// stale callback and a later one restores the member.
func DepartGoneHosts(ctx context.Context, tx store.Tx, gone []HostedMember, now time.Time) (bool, error) {
	changed := false
	for _, m := range gone {
		res, err := tx.ExecContext(ctx, `DELETE FROM member_host AS h WHERE h.principal_id = ?1 AND h.pid = ?2
			AND h.start_identity = ?3 AND h.pid_namespace = ?4 AND NOT `+governingLaunch+`
			AND EXISTS (SELECT 1 FROM member_selection s JOIN binding b ON b.id = s.binding_id
				WHERE s.principal_id = ?1 AND s.binding_id = ?5 AND s.generation = ?7 AND b.route_epoch = ?6 AND b.state = 'live')`,
			m.PrincipalID, m.PID, m.StartIdentity, m.PIDNamespace, m.BindingID, m.BindingEpoch, m.MembershipGeneration)
		if err != nil {
			return false, err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			if err != nil {
				return false, err
			}
			continue
		}
		b, found, err := bindingOf(ctx, tx, m.PrincipalID)
		if err != nil {
			return false, err
		}
		if !found || b.id != m.BindingID {
			continue
		}
		if err := endSessionBy(ctx, tx, b, m.PrincipalID, "process", now); err != nil {
			return false, err
		}
		if err := endOpenLaunches(ctx, tx, m.PrincipalID, now); err != nil {
			return false, err
		}
		changed = true
	}
	return changed, nil
}

// endOpenLaunches ends the launches a host departure leaves open, none of
// which is governing, and what depends on them.
func endOpenLaunches(ctx context.Context, tx store.Tx, principal int64, now time.Time) error {
	rows, err := tx.QueryContext(ctx, `SELECT launch_ref, json_extract(profile, '$.adapter') FROM adapter_launch
		WHERE principal_id = ? AND ended_hook_at_ms IS NULL AND fenced = 0`, principal)
	if err != nil {
		return err
	}
	var refs, adapters []string
	for rows.Next() {
		var ref string
		var adapter sql.NullString
		if err := rows.Scan(&ref, &adapter); err != nil {
			rows.Close()
			return err
		}
		refs = append(refs, ref)
		if adapter.Valid && !slices.Contains(adapters, adapter.String) {
			adapters = append(adapters, adapter.String)
		}
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE adapter_launch SET ended_hook_at_ms = MAX(?, started_hook_at_ms)
		WHERE principal_id = ? AND ended_hook_at_ms IS NULL AND fenced = 0`, ms(now), principal); err != nil {
		return err
	}
	for _, ref := range refs {
		if err := endLaunchPublishRoutes(ctx, tx, principal, ref, now); err != nil {
			return err
		}
	}
	for _, adapter := range adapters {
		if err := refreshMemberAdapterCapabilities(ctx, tx, principal, adapter); err != nil {
			return err
		}
	}
	return nil
}
