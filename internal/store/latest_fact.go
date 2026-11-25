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
//
// Facts count only from the latest start of the latest launch (a reused
// session-scoped reference starts over at a resume), only when the hook's
// clock was within the skew allowance of the dispatcher's when stored, and in
// causal order: a later turn outranks an earlier one whatever the clocks say,
// and within a turn the later stage wins. A turn-bound fact is placed at the
// time its turn opened, whatever its own clock says; any other fact by its
// own time.
//
// The read starts from the newest turn and the newest facts and goes no
// further back than the winner's position, so its cost does not grow with
// the launch's turns (#366).
func ReadLatestFact(ctx context.Context, tx Tx, principal, slackMS int64) (LatestFact, bool, error) {
	var launch string
	var start int64
	var live bool
	err := tx.QueryRowContext(ctx, `SELECT launch_ref, started_hook_at_ms, ended_hook_at_ms IS NULL AND fenced = 0
		FROM adapter_launch WHERE principal_id = ?
		ORDER BY ended_hook_at_ms IS NOT NULL, started_hook_at_ms DESC, rowid DESC LIMIT 1`, principal).Scan(&launch, &start, &live)
	if errors.Is(err, sql.ErrNoRows) {
		return LatestFact{}, false, nil
	}
	if err != nil {
		return LatestFact{}, false, err
	}
	// The latest turn opened since the start that has a fact.
	position, found := start, false
	var opened int64
	err = tx.QueryRowContext(ctx, latestTurnQuery, principal, launch, slackMS, start).Scan(&opened)
	switch {
	case err == nil:
		position, found = opened, true
	case !errors.Is(err, sql.ErrNoRows):
		return LatestFact{}, false, err
	}
	// A fact that no turn row places counts at its own time: only one at or
	// after that turn can tie or outrank it.
	var own int64
	err = tx.QueryRowContext(ctx, ownTimeQuery, principal, launch, slackMS, position).Scan(&own)
	switch {
	case err == nil:
		position, found = own, true
	case !errors.Is(err, sql.ErrNoRows):
		return LatestFact{}, false, err
	}
	if !found {
		return LatestFact{}, false, nil
	}
	// Among the facts at that position, the later stage, then the later clock.
	var f LatestFact
	err = tx.QueryRowContext(ctx, pickQuery, principal, launch, slackMS, position).
		Scan(&f.Seq, &f.Fact, &f.Source, &f.Profile, &f.Adapter, &f.AtMS, &f.FreshUntilMS, &f.CommandSince)
	if err != nil {
		return LatestFact{}, false, err
	}
	f.LaunchLive = live
	return f, true, nil
}

// The queries of ReadLatestFact take ?1 principal, ?2 launch, ?3 slack and
// ?4 the position they start from. Each reads through an index from the
// newest rows down (TestTheLatestFactQueriesReadFromTheNewestRows).
const (
	// latestTurnQuery: the latest turn opened at or after ?4 that has a fact.
	latestTurnQuery = `SELECT t.opened_hook_at_ms FROM turn t
		WHERE t.principal_id = ?1 AND t.launch_ref = ?2 AND t.opened_hook_at_ms >= ?4
			AND EXISTS (SELECT 1 FROM observation o WHERE ` + candidateFact + ` AND o.turn_ref = t.turn_ref)
		ORDER BY t.opened_hook_at_ms DESC LIMIT 1`
	// ownTimeQuery: the latest fact at or after ?4 that no turn row places
	// and that is its turn's latest stage.
	ownTimeQuery = `SELECT o.hook_at_ms FROM observation o
		WHERE ` + candidateFact + ` AND o.hook_at_ms >= ?4 AND ` + placedByItself + ` AND ` + latestStage + `
		ORDER BY o.hook_at_ms DESC LIMIT 1`
	// pickQuery reads the winner at position ?4: the latest stage, then the
	// later clock, then the later store.
	pickQuery = `WITH pick AS (
			SELECT seq FROM (
				-- CROSS JOIN keeps the turn first: from the turns at that position.
				SELECT o.seq, ` + stageO + ` AS stage, o.hook_at_ms FROM turn t CROSS JOIN observation o ON o.principal_id = t.principal_id
					AND o.launch_ref = t.launch_ref AND o.turn_ref = t.turn_ref
				WHERE t.principal_id = ?1 AND t.launch_ref = ?2 AND t.opened_hook_at_ms = ?4 AND ` + candidateFact + `
				UNION ALL
				SELECT o.seq, ` + stageO + `, o.hook_at_ms FROM observation o
				WHERE ` + candidateFact + ` AND o.hook_at_ms = ?4 AND ` + placedByItself + ` AND ` + latestStage + `) tied
			ORDER BY stage DESC, hook_at_ms DESC, seq DESC LIMIT 1)
		SELECT o.seq, o.kind, o.source, o.profile, COALESCE(json_extract(o.profile, '$.adapter'), ''), o.hook_at_ms, o.fresh_until_ms,
			EXISTS (SELECT 1 FROM observation k WHERE k.principal_id = ?1 AND k.kind = 'command' AND k.seq > o.seq)
		FROM pick JOIN observation o ON o.seq = pick.seq`
)

// candidateFact selects o, an adapter fact of launch ?2 of principal ?1
// whose hook clock was within ?3 of the dispatcher's when stored.
const candidateFact = `o.principal_id = ?1 AND o.launch_ref = ?2 AND o.kind NOT IN ('command', 'idle_prompt')
	AND o.hook_at_ms <= o.observed_at_ms + ?3`

// placedByItself: no turn row of the launch places o.
const placedByItself = `NOT EXISTS (SELECT 1 FROM turn p WHERE p.principal_id = ?1 AND p.launch_ref = ?2 AND p.turn_ref = o.turn_ref)`

// latestStage: o has no turn, or no fact of its turn is at a later stage.
const latestStage = `(o.turn_ref IS NULL OR NOT EXISTS (SELECT 1 FROM observation h
	WHERE h.principal_id = ?1 AND h.launch_ref = ?2 AND h.turn_ref = o.turn_ref AND h.kind NOT IN ('command', 'idle_prompt')
		AND h.hook_at_ms <= h.observed_at_ms + ?3 AND ` + stageH + ` > ` + stageO + `))`

// stageO and stageH rank a fact's kind within its turn: a later stage wins.
const (
	stageO = `CASE o.kind WHEN 'session_ended' THEN 4 WHEN 'turn_finishing' THEN 3 WHEN 'approval_requested' THEN 2 ELSE 1 END`
	stageH = `CASE h.kind WHEN 'session_ended' THEN 4 WHEN 'turn_finishing' THEN 3 WHEN 'approval_requested' THEN 2 ELSE 1 END`
)

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
