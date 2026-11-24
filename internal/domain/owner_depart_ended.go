package domain

import (
	"context"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

const VerbOwnerDepartEnded = "owner depart ended"

// endedDepartAfter is how long a live member's session must have been over,
// with nothing seen from it since, before the owner is told and may depart
// it with owner depart --ended.
const endedDepartAfter = 12 * time.Hour

// routeSessionEnded is the reason of a route its session's end closed.
const routeSessionEnded = "session ended"

// OwnerDepartEnded departs every live member whose session ended more than
// endedDepartAfter ago, with nothing seen from it since, that has no open
// request: none it owes a reply to and none it awaits, in any channel.
type OwnerDepartEnded struct{}

func (OwnerDepartEnded) command()     {}
func (OwnerDepartEnded) Verb() string { return VerbOwnerDepartEnded }

// EndedMember is a live member whose session ended long ago.
type EndedMember struct {
	BindingID    int64   `json:"binding_id"`
	Channel      string  `json:"channel"`
	Alias        string  `json:"alias"`
	EndedAtMS    int64   `json:"ended_at_ms"`
	OpenRequests []int64 `json:"open_request_ids,omitempty"`

	channel, principal int64
	vendor             string
}

// OwnerDepartEndedResult lists the members departed, and those left live
// because they still have open requests.
type OwnerDepartEndedResult struct {
	Departed []EndedMember `json:"departed"`
	Owing    []EndedMember `json:"owing,omitempty"`
}

// endedMembers lists the live members whose route their session's end
// closed more than endedDepartAfter ago, with no fact or command of theirs
// since (one in the same millisecond counts as since). A Codex session end is not proof the window closed (an unloaded
// thread may be idle), so these are reported, never departed unasked.
func endedMembers(ctx context.Context, tx store.Tx, now time.Time) ([]EndedMember, error) {
	rows, err := tx.QueryContext(ctx, `SELECT b.id, b.channel_id, b.principal_id, c.name, b.alias, p.vendor, r.updated_at_ms
		FROM binding b JOIN channel c ON c.id = b.channel_id JOIN principal p ON p.id = b.principal_id
		JOIN publish_route r ON r.binding_id = b.id
		WHERE b.state = 'live' AND r.state = 'ended' AND r.reason = ? AND r.updated_at_ms < ?
			AND NOT EXISTS (SELECT 1 FROM observation o WHERE o.principal_id = b.principal_id
				AND o.observed_at_ms >= r.updated_at_ms AND o.kind <> 'session_ended')
		ORDER BY c.name, b.alias`, routeSessionEnded, ms(now.Add(-endedDepartAfter)))
	if err != nil {
		return nil, err
	}
	var out []EndedMember
	for rows.Next() {
		var m EndedMember
		if err := rows.Scan(&m.BindingID, &m.channel, &m.principal, &m.Channel, &m.Alias, &m.vendor, &m.EndedAtMS); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for i := range out {
		if out[i].OpenRequests, err = memberOpenRequests(ctx, tx, out[i].principal); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *Service) ownerDepartEnded(ctx context.Context, tx store.Tx, caller core.Caller, req Request, cmd OwnerDepartEnded, now time.Time) (core.Result, error) {
	if caller.Role != core.RoleOwner {
		return Refuse(core.CodeOwnerOnly, "only the owner can depart a binding", "run it from the owner's own shell, not an agent session"), nil
	}
	owner, err := ownerPrincipal(ctx, tx, now)
	if err != nil {
		return core.Result{}, err
	}
	const scope = "owner:depart-ended"
	if result, found, err := replay(ctx, tx, owner, req.OpKey, scope, cmd); found || err != nil {
		return result, err
	}
	members, err := endedMembers(ctx, tx, now)
	if err != nil {
		return core.Result{}, err
	}
	out := OwnerDepartEndedResult{Departed: []EndedMember{}}
	for _, m := range members {
		if len(m.OpenRequests) > 0 {
			out.Owing = append(out.Owing, m)
			continue
		}
		departed := OwnerDepartResult{BindingID: m.BindingID, Channel: m.Channel, Alias: m.Alias}
		if err := departByOwner(ctx, tx, owner, m.channel, m.principal, m.vendor, departed, now); err != nil {
			return core.Result{}, err
		}
		out.Departed = append(out.Departed, m)
	}
	result, err := OK(out)
	if err != nil {
		return core.Result{}, err
	}
	return result, record(ctx, tx, owner, req.OpKey, scope, cmd, result, now)
}

// memberOpenRequests lists the open requests principal takes part in, in
// any channel: those it owes a reply to and those it asked and still
// awaits. A member with any of them is not departed by depart --ended.
func memberOpenRequests(ctx context.Context, tx store.Tx, principal int64) ([]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT o.message_id FROM obligation o WHERE o.state = 'open' AND o.principal_id = ?1
		UNION SELECT o.message_id FROM obligation o JOIN message m ON m.id = o.message_id
		WHERE o.state = 'open' AND m.sender_id = ?1 ORDER BY 1`, principal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
