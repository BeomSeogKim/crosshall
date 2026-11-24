package domain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

type member struct {
	principal, binding, channel int64
	channelName, alias          string
}

// liveMember resolves the caller to its live binding, or returns a result to
// send back instead (not enrolled, archived channel).
func liveMember(ctx context.Context, tx store.Tx, caller core.Caller) (member, *core.Result, error) {
	var m member
	if caller.Role != core.RoleMember || caller.PrincipalID == nil {
		r := notEnrolled()
		return m, &r, nil
	}
	var state string
	var generation int64
	err := tx.QueryRowContext(ctx,
		`SELECT b.id, b.channel_id, c.name, c.state, b.alias, s.generation
		 FROM member_selection s JOIN binding b ON b.id = s.binding_id JOIN channel c ON c.id = b.channel_id
		 WHERE s.principal_id = ? AND b.state = 'live'`, *caller.PrincipalID).
		Scan(&m.binding, &m.channel, &m.channelName, &state, &m.alias, &generation)
	if errors.Is(err, sql.ErrNoRows) {
		r := notEnrolled()
		return m, &r, nil
	}
	if err != nil {
		return m, nil, err
	}
	if caller.BindingID != nil && *caller.BindingID != m.binding ||
		caller.Membership != nil && (caller.Membership.BindingID != m.binding || caller.Membership.Generation != generation) {
		r := Refuse(core.CodeBindingChanged, "the session's channel changed before this command ran", "run the command again from the current session")
		return m, &r, nil
	}
	m.principal = *caller.PrincipalID
	if state != "active" {
		r := Refuse(core.CodeArchived, fmt.Sprintf("#%s is archived", m.channelName), "move this session to an active channel with crosshall join <channel> --move")
		return m, &r, nil
	}
	return m, nil, nil
}

func askParent(ctx context.Context, tx store.Tx, m member, id int64) (int64, *core.Result, error) {
	if id == 0 {
		return 0, nil, nil
	}
	var kind string
	var parent int64
	var party bool
	err := tx.QueryRowContext(ctx, `SELECT kind, COALESCE(in_reply_to, id), sender_id = ?1 OR EXISTS (
        SELECT 1 FROM recipient WHERE message_id = message.id AND principal_id = ?1)
        FROM message WHERE id = ?2 AND channel_id = ?3`, m.principal, id, m.channel).Scan(&kind, &parent, &party)
	if errors.Is(err, sql.ErrNoRows) {
		r := Refuse(core.CodeNotFound, fmt.Sprintf("no message #%d in #%s", id, m.channelName), "crosshall log shows the channel's message IDs")
		return 0, &r, nil
	}
	if err != nil {
		return 0, nil, err
	}
	if kind == "note" || !party {
		r := Refuse(core.CodeBadRoot, fmt.Sprintf("#%d is not a request or reply you sent or received", id),
			"crosshall waiting and crosshall inbox list your requests")
		return 0, &r, nil
	}
	return parent, nil, nil
}

func exchangeForRoot(ctx context.Context, tx store.Tx, root int64) (int64, error) {
	var id int64
	err := tx.QueryRowContext(ctx, `SELECT exchange_id FROM message WHERE id = ?`, root).Scan(&id)
	return id, err
}

func open(ctx context.Context, tx store.Tx, m member, parentRequestID int64) (int64, error) {
	// The root message does not exist yet; a unique negative placeholder
	// satisfies the deferred foreign key until it is set below.
	res, err := tx.ExecContext(ctx,
		`INSERT INTO exchange (channel_id, root_message_id, opened_by_id, granted, parent_request_id)
		 VALUES (?, -1 - (SELECT COALESCE(MAX(id), 0) FROM exchange), ?, 0, NULLIF(?, 0))`,
		m.channel, m.principal, parentRequestID)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}
