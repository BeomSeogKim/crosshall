package domain

import (
	"context"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

// approvalSettle is how long an approval request stands before its
// server is asked: a vendor that approves on its own answers within it.
const approvalSettle = 2 * time.Second

// approvalRefresh is how close to its cap a confirmed approval is asked
// again, so a dialog still open does not age out (R3: a detached turn
// waits on its owner for as long as the owner is away).
const approvalRefresh = 5 * time.Minute

// ApprovalCheck is a member on a thread route whose latest fact is an
// approval request its vendor's server should confirm: its thread, the
// server its route named, and the home its route names.
type ApprovalCheck struct {
	Principal int64
	FactSeq   int64
	Target    PublishTarget
	RouteHome string
}

// ApprovalChecks lists members on an active thread route whose latest
// fact is an approval request that has stood approvalSettle and is not
// confirmed yet, or a confirmed one within approvalRefresh of its cap.
func ApprovalChecks(ctx context.Context, tx store.Tx, now time.Time) ([]ApprovalCheck, error) {
	nowMS := ms(now)
	rows, err := tx.QueryContext(ctx, `SELECT r.principal_id, r.adapter, r.thread_id, r.vendor_home, r.host_pid, r.host_start
		FROM publish_route r WHERE r.state = 'active' AND r.thread_id IS NOT NULL AND r.vendor_home IS NOT NULL
			AND r.host_pid IS NOT NULL AND r.host_start IS NOT NULL
			AND EXISTS (SELECT 1 FROM observation o WHERE o.principal_id = r.principal_id
				AND o.kind = 'approval_requested' AND o.observed_at_ms >= ?)
		ORDER BY r.principal_id`, nowMS-2*factCapMS[FactApprovalRequested])
	if err != nil {
		return nil, err
	}
	var routes []ApprovalCheck
	for rows.Next() {
		var c ApprovalCheck
		if err := rows.Scan(&c.Principal, &c.Target.Adapter, &c.Target.Address, &c.RouteHome,
			&c.Target.Queue.PID, &c.Target.Queue.StartMS); err != nil {
			rows.Close()
			return nil, err
		}
		routes = append(routes, c)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	var out []ApprovalCheck
	for _, c := range routes {
		f, found, err := store.ReadLatestFact(ctx, tx, c.Principal, futureSlackMS)
		if err != nil {
			return nil, err
		}
		if !found || f.Fact != FactApprovalRequested || !f.LaunchLive || f.CommandSince {
			continue
		}
		confirmed := f.Source == store.ApprovalShownSource
		switch {
		case !confirmed && nowMS-f.AtMS < approvalSettle.Milliseconds():
			continue
		case confirmed && (!f.FreshUntilMS.Valid || f.FreshUntilMS.Int64-nowMS > approvalRefresh.Milliseconds()):
			continue
		}
		c.FactSeq = f.Seq
		out = append(out, c)
	}
	return out, nil
}

// ConfirmApproval records that the member's server shows its approval
// request waiting on its owner: a copy of the request, stored by the
// dispatcher with a fresh cap, so the views treat it as an open approval.
// Nothing is stored when the member's latest fact is no longer that one.
func ConfirmApproval(ctx context.Context, tx store.Tx, principal, factSeq int64, now time.Time) (bool, error) {
	f, found, err := store.ReadLatestFact(ctx, tx, principal, futureSlackMS)
	if err != nil || !found || f.Seq != factSeq || f.Fact != FactApprovalRequested {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO observation
		(principal_id, kind, origin, source, observed_at_ms, fresh_until_ms, launch_ref, event_ref, turn_ref, hook_at_ms, profile)
		SELECT principal_id, kind, origin, ?, ?, ?, launch_ref, NULL, turn_ref, ?, profile FROM observation WHERE seq = ?`,
		store.ApprovalShownSource, ms(now), ms(now)+factCapMS[FactApprovalRequested], ms(now), factSeq)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
