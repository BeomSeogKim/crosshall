package domain

import (
	"context"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

// ProcessLaunch is an open launch whose session process the sweep checks:
// verified, with a process reference, of a manifest revision that declares
// process_launch (store.ProcessLaunchProfile).
type ProcessLaunch struct {
	RowID, PrincipalID, BindingID, BindingEpoch   int64
	MembershipGeneration                          int64
	Adapter, LaunchRef, PIDNamespace              string
	StartedAtMS, ProcessPID, ProcessStartIdentity int64
}

type ProcessLaunchProbe struct {
	ProcessLaunch
	PID, StartIdentity int64
	Gone, PreviousBoot bool
}

func LiveProcessLaunches(ctx context.Context, tx store.Tx, afterRowID int64, limit int) ([]ProcessLaunch, error) {
	rows, err := tx.QueryContext(ctx, `SELECT l.rowid, l.principal_id, b.id, b.route_epoch, s.generation,
		json_extract(l.profile, '$.adapter'), l.launch_ref, l.started_hook_at_ms,
		COALESCE(l.process_pid, 0), COALESCE(l.process_start_identity, 0), l.pid_namespace
		FROM adapter_launch l JOIN member_selection s ON s.principal_id = l.principal_id JOIN binding b ON b.id = s.binding_id
		WHERE l.rowid > ? AND b.state = 'live' AND l.verified = 1 AND l.fenced = 0 AND l.ended_hook_at_ms IS NULL
		AND `+processLaunchProfile("l.profile")+` AND l.launch_ref GLOB 'pid-*'
		ORDER BY l.rowid LIMIT ?`, afterRowID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var launches []ProcessLaunch
	for rows.Next() {
		var l ProcessLaunch
		if err := rows.Scan(&l.RowID, &l.PrincipalID, &l.BindingID, &l.BindingEpoch, &l.MembershipGeneration, &l.Adapter, &l.LaunchRef, &l.StartedAtMS,
			&l.ProcessPID, &l.ProcessStartIdentity, &l.PIDNamespace); err != nil {
			return nil, err
		}
		launches = append(launches, l)
	}
	return launches, rows.Err()
}

var processLaunchProfile = store.ProcessLaunchProfile

var sameProcessLaunch = `WHERE rowid = ?1 AND principal_id = ?2 AND launch_ref = ?3 AND started_hook_at_ms = ?4
	AND EXISTS (SELECT 1 FROM member_selection s JOIN binding b ON b.id = s.binding_id
		WHERE s.principal_id = ?2 AND s.binding_id = ?5 AND s.generation = ?10 AND b.route_epoch = ?6 AND b.state = 'live')
	AND verified = 1 AND fenced = 0 AND ended_hook_at_ms IS NULL AND ` + processLaunchProfile("profile") + `
	AND COALESCE(process_pid, 0) = ?7 AND COALESCE(process_start_identity, 0) = ?8 AND pid_namespace = ?9`

type affectedProcessMember struct {
	binding hookBinding
	before  *Observed
	last    ProcessLaunch
}

func ApplyProcessLaunchProbes(ctx context.Context, tx store.Tx, results []ProcessLaunchProbe, now time.Time) (bool, error) {
	changed := false
	affected := make(map[int64]*affectedProcessMember)
	var principals []int64
	for _, result := range results {
		l := result.ProcessLaunch
		args := []any{l.RowID, l.PrincipalID, l.LaunchRef, l.StartedAtMS, l.BindingID, l.BindingEpoch, l.ProcessPID, l.ProcessStartIdentity, l.PIDNamespace, l.MembershipGeneration}
		if !result.Gone {
			if l.ProcessPID != 0 || l.ProcessStartIdentity != 0 || result.PID <= 0 || result.StartIdentity <= 0 {
				continue
			}
			res, err := tx.ExecContext(ctx, `UPDATE adapter_launch SET process_pid = ?11, process_start_identity = ?12 `+sameProcessLaunch+
				` AND (launch_ref = 'pid-' || ?11 OR launch_ref GLOB 'pid-' || ?11 || '-*')`, append(args, result.PID, result.StartIdentity)...)
			if err != nil {
				return false, err
			}
			if n, err := res.RowsAffected(); err != nil {
				return false, err
			} else if n != 0 {
				changed = true
			}
			continue
		}
		member := affected[l.PrincipalID]
		if member == nil {
			b, found, err := bindingOf(ctx, tx, l.PrincipalID)
			if err != nil {
				return false, err
			}
			if !found || b.state != "live" || b.id != l.BindingID || b.routeEpoch != l.BindingEpoch || b.selectionGen != l.MembershipGeneration {
				continue
			}
			before, err := presenceOf(ctx, tx, l.PrincipalID, now)
			if err != nil {
				return false, err
			}
			member = &affectedProcessMember{binding: b, before: before, last: l}
		}
		res, err := tx.ExecContext(ctx, `UPDATE adapter_launch SET ended_hook_at_ms = MAX(?11, started_hook_at_ms), ended_by_process = 1 `+
			sameProcessLaunch, append(args, ms(now))...)
		if err != nil {
			return false, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return false, err
		}
		if n == 0 {
			continue
		}
		if affected[l.PrincipalID] == nil {
			affected[l.PrincipalID] = member
			principals = append(principals, l.PrincipalID)
		}
		if l.StartedAtMS > member.last.StartedAtMS || l.StartedAtMS == member.last.StartedAtMS && l.RowID > member.last.RowID {
			member.last = l
		}
		if result.PreviousBoot {
			if err := retireLaunchPublish(ctx, tx, l.BindingID, l.PrincipalID, l.LaunchRef, now); err != nil {
				return false, err
			}
		} else if l.ProcessPID > 0 && l.ProcessStartIdentity > 0 {
			if err := RetirePublishQueue(ctx, tx, PublishQueue{PID: l.ProcessPID, StartMS: l.ProcessStartIdentity}, now); err != nil {
				return false, err
			}
		}
		if err := endLaunchPublishRoutes(ctx, tx, l.PrincipalID, l.LaunchRef, now); err != nil {
			return false, err
		}
		changed = true
	}
	for _, principal := range principals {
		member := affected[principal]
		if err := finishProcessExit(ctx, tx, principal, member, now); err != nil {
			return false, err
		}
	}
	return changed, nil
}

func finishProcessExit(ctx context.Context, tx store.Tx, principal int64, member *affectedProcessMember, now time.Time) error {
	var possibleSuccessor bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM adapter_launch WHERE principal_id = ?
		AND ended_hook_at_ms IS NULL AND fenced = 0 AND (verified = 1 OR (started_hook_at_ms, rowid) > (?, ?)))`,
		principal, member.last.StartedAtMS, member.last.RowID).Scan(&possibleSuccessor); err != nil {
		return err
	}
	if possibleSuccessor {
		if err := presenceChanged(ctx, tx, member.binding.channel, principal, member.before, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO adapter_dirty (principal_id, notice, reminder) VALUES (?, 1, 1)
			ON CONFLICT(principal_id) DO UPDATE SET notice = 1, reminder = 1`, principal); err != nil {
			return err
		}
	} else if err := endSession(ctx, tx, member.binding, principal, now); err != nil {
		return err
	}
	return refreshMemberAdapterCapabilities(ctx, tx, principal, member.last.Adapter)
}
