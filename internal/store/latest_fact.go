package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// FactFutureSlackMS bounds how far ahead of the dispatcher a hook's clock may
// be before its fact is stored but not projected.
const FactFutureSlackMS = 5_000

// LatestFact is a principal's effective adapter fact as stored.
type LatestFact struct {
	// Seq is the observation's sequence number.
	Seq  int64
	Fact string
	// Source is who stored it: a hook ("adapter:<vendor>@<version>"), or
	// ApprovalShownSource.
	Source       string
	Profile      string
	Adapter      string
	AtMS         int64
	FreshUntilMS sql.NullInt64
	// LaunchLive says the fact's launch has not ended and is not fenced:
	// no session end and no sign that its process is gone.
	LaunchLive bool
	// CommandSince says the member ran a crosshall command after the fact
	// was stored: its session is acting again.
	CommandSince bool
}

// ReadLatestFact returns the principal's effective fact: the latest, in
// causal order, among the observations of its latest launch. found is false
// when there is no adapter fact. A hook clock more than slackMS ahead of the
// dispatcher's when stored does not count.
func ReadLatestFact(ctx context.Context, tx Tx, principal, slackMS int64) (LatestFact, bool, error) {
	var f LatestFact
	// Facts count only from the latest start of the latest launch (a reused
	// session-scoped reference starts over at a resume), only when the hook's
	// clock was within the skew allowance of the dispatcher's when stored, and
	// in causal order: a later turn outranks an earlier one whatever the
	// clocks say, and within a turn the later stage wins.
	err := tx.QueryRowContext(ctx, `
		WITH latest AS (SELECT launch_ref, started_hook_at_ms, ended_hook_at_ms IS NULL AND fenced = 0 AS live
		                FROM adapter_launch WHERE principal_id = ?1
		                ORDER BY ended_hook_at_ms IS NOT NULL, started_hook_at_ms DESC, rowid DESC LIMIT 1),
		candidate AS (
			SELECT o.seq, o.kind, o.profile, o.hook_at_ms, o.fresh_until_ms, o.turn_ref, o.launch_ref,
			       CASE o.kind WHEN 'session_ended' THEN 4 WHEN 'turn_finishing' THEN 3
			                   WHEN 'approval_requested' THEN 2 ELSE 1 END AS stage,
			       COALESCE((SELECT t.opened_hook_at_ms FROM turn t WHERE t.principal_id = o.principal_id
			                 AND t.launch_ref = o.launch_ref AND t.turn_ref = o.turn_ref), o.hook_at_ms) AS position
			FROM observation o JOIN latest ON o.launch_ref = latest.launch_ref
			WHERE o.principal_id = ?1 AND o.kind NOT IN ('command', 'idle_prompt')
			  AND o.hook_at_ms <= o.observed_at_ms + ?2),
		staged AS (
			SELECT c.*, MAX(c.stage) OVER (PARTITION BY c.turn_ref) AS top FROM candidate c),
		pick AS (
			SELECT c.seq, c.kind, c.profile, c.hook_at_ms, c.fresh_until_ms FROM staged c
			-- A turn-bound fact belongs to the start its turn opened after, whatever
			-- its own clock says; any other fact is placed by its own time. Within
			-- a turn only its latest stage counts.
			WHERE c.position >= (SELECT started_hook_at_ms FROM latest) AND (c.turn_ref IS NULL OR c.stage = c.top)
			ORDER BY c.position DESC, c.stage DESC, c.hook_at_ms DESC, c.seq DESC
			LIMIT 1)
		-- The rest is read for the chosen fact only: per candidate it made a
		-- hook event's cost grow with the square of the session's turns.
		SELECT p.seq, p.kind, (SELECT source FROM observation WHERE seq = p.seq), p.profile, COALESCE(json_extract(p.profile, '$.adapter'), ''), p.hook_at_ms, p.fresh_until_ms,
		       (SELECT live FROM latest),
		       EXISTS (SELECT 1 FROM observation k WHERE k.principal_id = ?1 AND k.kind = 'command' AND k.seq > p.seq)
		FROM pick p`, principal, slackMS).
		Scan(&f.Seq, &f.Fact, &f.Source, &f.Profile, &f.Adapter, &f.AtMS, &f.FreshUntilMS, &f.LaunchLive, &f.CommandSince)
	if errors.Is(err, sql.ErrNoRows) {
		return LatestFact{}, false, nil
	}
	return f, err == nil, err
}

// DoctorApproval is a live member whose turn waits on its owner's approval.
type DoctorApproval struct {
	Alias, Channel string
	AtMS           int64
	// Current: within the fact's cap. Past it no answer was reported, but a
	// denied dialog reports none either.
	Current bool
}

// readDoctorApprovals lists live members whose latest fact is an open
// approval, by the same test the dispatcher's views use.
func readDoctorApprovals(ctx context.Context, tx Tx, nowMS int64) ([]DoctorApproval, error) {
	rows, err := tx.QueryContext(ctx, `SELECT b.principal_id, b.alias, c.name FROM binding b
		JOIN channel c ON c.id = b.channel_id
		JOIN member_selection s ON s.principal_id = b.principal_id AND s.binding_id = b.id
		WHERE b.state = 'live' ORDER BY c.name, b.alias`)
	if err != nil {
		return nil, err
	}
	type member struct {
		principal      int64
		alias, channel string
	}
	var members []member
	for rows.Next() {
		var m member
		if err := rows.Scan(&m.principal, &m.alias, &m.channel); err != nil {
			rows.Close()
			return nil, err
		}
		members = append(members, m)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return nil, err
	}
	var out []DoctorApproval
	for _, m := range members {
		f, found, err := ReadLatestFact(ctx, tx, m.principal, FactFutureSlackMS)
		if err != nil {
			return nil, err
		}
		if found && f.ApprovalOpen() {
			current := !f.FreshUntilMS.Valid || nowMS < f.FreshUntilMS.Int64
			out = append(out, DoctorApproval{Alias: m.alias, Channel: m.channel, AtMS: f.AtMS, Current: current})
		}
	}
	return out, nil
}

// approvalIsDialog: the fact's own profile, at its exact manifest
// revision, declares that its approval fact means a shown permission dialog.
// Claude Code reports Notification(permission_prompt), sent only when it
// shows the dialog. Codex's PermissionRequest also runs for escalations it
// approves on its own, most followed by the member's next fact within two
// minutes, so its fact says nothing about an open dialog. A revision the
// build does not know counts as a request.
func (f LatestFact) approvalIsDialog() bool {
	var profile wire.HookProfile
	if json.Unmarshal([]byte(f.Profile), &profile) != nil || profile.Adapter != f.Adapter {
		return false
	}
	caps, _ := manifests.CapabilitiesAt(profile.Adapter, profile.ManifestRev)
	return caps.ApprovalDialog
}

// ApprovalShownSource marks an approval fact the dispatcher stored after
// the vendor's own server showed the request waiting on its owner, for an
// adapter whose hook fact alone does not mean a shown dialog.
const ApprovalShownSource = "appserver"

// ApprovalOpen: the fact is an approval request that is known to wait on
// the owner (a revision whose request means a shown dialog, or one the
// vendor's server confirmed), its launch is not known to have ended, and
// the member has run no crosshall command since.
func (f LatestFact) ApprovalOpen() bool {
	return f.Fact == "approval_requested" && f.LaunchLive && !f.CommandSince &&
		(f.Source == ApprovalShownSource || f.approvalIsDialog())
}
