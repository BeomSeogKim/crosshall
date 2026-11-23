package domain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

type membershipSelection struct {
	member
	state         string
	ownerFence    sql.NullInt64
	ownerDeparted bool
}

func selectedMembership(ctx context.Context, tx store.Tx, principal int64) (membershipSelection, error) {
	selected := membershipSelection{member: member{principal: principal}}
	err := tx.QueryRowContext(ctx, `SELECT b.id, c.id, c.name, b.alias, b.state, b.owner_hook_fence_ms, b.departed_by_owner
		FROM member_selection s JOIN binding b ON b.id = s.binding_id JOIN channel c ON c.id = b.channel_id
		WHERE s.principal_id = ?`, principal).
		Scan(&selected.binding, &selected.channel, &selected.channelName, &selected.alias, &selected.state, &selected.ownerFence, &selected.ownerDeparted)
	if errors.Is(err, sql.ErrNoRows) {
		return selected, nil
	}
	return selected, err
}

// ReplayMembershipChange checks the recorded scope rather than the current
// channel. A retried leave or join must never act on a different membership.
func ReplayMembershipChange(ctx context.Context, tx store.Tx, principal int64, key string, cmd Command) (core.Result, bool, error) {
	if principal == 0 || key == "" {
		return core.Result{}, false, nil
	}
	switch cmd := cmd.(type) {
	case Join:
		if cmd.RepoRoot != "" {
			return replayRepoJoin(ctx, tx, principal, key, "repo:"+cmd.RepoRoot, cmd)
		}
	case Leave:
	default:
		return core.Result{}, false, nil
	}
	var scope string
	err := tx.QueryRowContext(ctx, `SELECT scope FROM op WHERE principal_id = ? AND op_key = ?`, principal, key).Scan(&scope)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Result{}, false, nil
	}
	if err != nil {
		return core.Result{}, false, err
	}
	return replay(ctx, tx, principal, key, scope, cmd)
}

func prepareChannelMove(ctx context.Context, tx store.Tx, source membershipSelection, now time.Time) (*JoinMoveReceipt, *core.Result, error) {
	attempt, err := pendingPublishForMove(ctx, tx, source.binding, source.principal)
	if err != nil {
		return nil, nil, err
	}
	if attempt != 0 {
		r := Refuse(core.CodeMoveNoticePending, fmt.Sprintf("notice %d in #%s still awaits a receipt", attempt, source.channelName),
			"retry the move after the notice is resolved; the owner can check it with crosshall owner doctor")
		return nil, &r, nil
	}
	ids, err := pendingIDs(ctx, tx, source.principal, source.channel)
	if err != nil {
		return nil, nil, err
	}
	out := &JoinMoveReceipt{Channel: source.channelName, Unread: len(ids)}
	if out.Owed, out.Waiting, err = openMembershipCounts(ctx, tx, source.member); err != nil {
		return nil, nil, err
	}
	if source.state == "live" {
		if _, err := tx.ExecContext(ctx, `UPDATE binding SET state = 'departed', departed_at_ms = ? WHERE id = ?`, ms(now), source.binding); err != nil {
			return nil, nil, err
		}
		if err := rosterChange(ctx, tx, source.channel, source.principal, "departed"); err != nil {
			return nil, nil, err
		}
		if err := audit(ctx, tx, "departed", "channel:"+source.channelName, source.principal, map[string]string{"alias": source.alias}, now); err != nil {
			return nil, nil, err
		}
	}
	return out, nil, nil
}

func openMembershipCounts(ctx context.Context, tx store.Tx, m member) (owed, waiting int, err error) {
	err = tx.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM obligation o JOIN message msg ON msg.id = o.message_id
		 WHERE o.principal_id = ?1 AND o.state = 'open' AND msg.channel_id = ?2),
		(SELECT COUNT(*) FROM obligation o JOIN message msg ON msg.id = o.message_id
		 WHERE msg.sender_id = ?1 AND o.state = 'open' AND msg.channel_id = ?2)`, m.principal, m.channel).Scan(&owed, &waiting)
	return owed, waiting, err
}
