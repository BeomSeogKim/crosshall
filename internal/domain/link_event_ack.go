package domain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/link"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

type linkCutSlot struct {
	seq uint64
	link.Member
}

func (s *Service) linkAck(ctx context.Context, tx store.Tx, caller core.Caller, cmd LinkAck, now time.Time) (core.Result, error) {
	peer, ack := cmd.LinkEventPeer, cmd.Ack
	if refusal, err := authorizeLinkEventPeer(ctx, tx, caller, peer); err != nil || refusal != nil {
		if refusal != nil {
			return *refusal, nil
		}
		return core.Result{}, err
	}
	stream, err := readLinkEventStream(ctx, tx, peer)
	if errors.Is(err, sql.ErrNoRows) {
		return refuseLinkEvents("acknowledgement requires an active subscription"), nil
	}
	if err != nil {
		return core.Result{}, err
	}
	cursor, err := readLinkEventCursor(ctx, tx, peer.Device)
	if err != nil {
		return core.Result{}, err
	}
	if ack.Boot != stream.boot || !validLinkInteger(ack.Seq) || ack.Seq > cursor.head ||
		stream.ackSeq.Valid && uint64(stream.ackSeq.Int64) != ack.Seq {
		return refuseLinkEvents("invalid acknowledgement cursor or boot"), nil
	}
	snapshot := stream.snapshotSeq.Valid
	if snapshot {
		if ack.Seq != uint64(stream.snapshotSeq.Int64) || ack.Seq < cursor.acked {
			return refuseLinkEvents("the acknowledgement does not match the current snapshot"), nil
		}
	} else {
		if ack.Boot != cursor.boot || peer.CredentialGen != cursor.credentialGen {
			return refuseLinkEvents("this boot needs a new snapshot"), nil
		}
		if ack.Seq <= cursor.acked {
			return OK(nil)
		}
		if cursor.acked < cursor.trimmed {
			return refuseLinkEvents("acknowledgement crosses a trimmed event"), nil
		}
	}
	if err := stageLinkCuts(ctx, tx, peer, ack, cursor.acked, snapshot); err != nil {
		if errors.Is(err, errLinkCutScope) {
			return refuseLinkEvents(err.Error()), nil
		}
		return core.Result{}, err
	}
	if ack.More {
		_, err := tx.ExecContext(ctx, `UPDATE link_event_stream SET ack_seq = ? WHERE device_id = ? AND stream = ?`,
			ack.Seq, peer.Device, peer.Stream)
		if err != nil {
			return core.Result{}, err
		}
		return OK(nil)
	}
	expected, err := expectedLinkCuts(ctx, tx, peer, cursor.acked, ack.Seq, snapshot)
	if err != nil {
		return core.Result{}, err
	}
	cuts, err := readStagedLinkCuts(ctx, tx, peer, expected)
	if errors.Is(err, errLinkCutScope) {
		return refuseLinkEvents(err.Error()), nil
	}
	if err != nil {
		return core.Result{}, err
	}
	if err := applyLinkCuts(ctx, tx, peer, ack.Boot, cuts); err != nil {
		if errors.Is(err, errLinkCutScope) {
			return refuseLinkEvents(err.Error()), nil
		}
		return core.Result{}, err
	}
	if snapshot {
		if _, err := tx.ExecContext(ctx, `UPDATE link_event_cursor SET boot = ?, credential_gen = ? WHERE device_id = ?`,
			ack.Boot, peer.CredentialGen, peer.Device); err != nil {
			return core.Result{}, err
		}
	}
	ack.Cuts = cuts
	if err := s.resolveLinkFactCuts(ctx, tx, peer, ack, snapshot, now); err != nil {
		return core.Result{}, err
	}
	if err := finishLinkAck(ctx, tx, peer, ack.Seq); err != nil {
		return core.Result{}, err
	}
	return OK(nil)
}

var errLinkCutScope = errors.New("the acknowledgement does not match the delivered leaf scopes")

func stageLinkCuts(ctx context.Context, tx store.Tx, peer LinkEventPeer, ack link.Ack, previous uint64, snapshot bool) error {
	for _, cut := range ack.Cuts {
		if cut.Vendor == "" || cut.Key == "" || cut.Scope.LeafGeneration == "" || !validLinkInteger(cut.LeafSeq) ||
			snapshot && cut.Seq != ack.Seq || !snapshot && (cut.Seq <= previous || cut.Seq > ack.Seq) {
			return errLinkCutScope
		}
		if cut.Absent {
			cut.LeafSeq = 0
		}
		data, err := json.Marshal(cut)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO link_event_cut_stage (device_id, stream, seq, vendor, key, body)
			VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING`, peer.Device, peer.Stream, cut.Seq, cut.Vendor, cut.Key, string(data))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			var stored string
			err := tx.QueryRowContext(ctx, `SELECT body FROM link_event_cut_stage
				WHERE device_id = ? AND stream = ? AND seq = ? AND vendor = ? AND key = ?`,
				peer.Device, peer.Stream, cut.Seq, cut.Vendor, cut.Key).Scan(&stored)
			if err != nil {
				return err
			}
			if stored != string(data) {
				return errLinkCutScope
			}
		}
	}
	return nil
}

func expectedLinkCuts(ctx context.Context, tx store.Tx, peer LinkEventPeer, previous, seq uint64, snapshot bool) (map[linkCutSlot]link.MemberScope, error) {
	if snapshot {
		rows, err := tx.QueryContext(ctx, `SELECT body FROM link_event_snapshot WHERE device_id = ? AND stream = ? ORDER BY part`,
			peer.Device, peer.Stream)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		cuts := make(map[linkCutSlot]link.MemberScope)
		for rows.Next() {
			var data string
			if err := rows.Scan(&data); err != nil {
				return nil, err
			}
			var part link.Snapshot
			if err := json.Unmarshal([]byte(data), &part); err != nil {
				return nil, err
			}
			for _, member := range part.Members {
				cuts[linkCutSlot{seq: seq, Member: member.Member}] = member.Scope
			}
		}
		return cuts, rows.Err()
	}
	rows, err := tx.QueryContext(ctx, `SELECT seq, body FROM device_event WHERE device_id = ? AND seq > ? AND seq <= ?
		AND kind IN (?, ?) ORDER BY seq`, peer.Device, previous, seq, link.EventMembership, link.EventInvalidate)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cuts := make(map[linkCutSlot]link.MemberScope)
	for rows.Next() {
		var eventSeq uint64
		var data string
		if err := rows.Scan(&eventSeq, &data); err != nil {
			return nil, err
		}
		var event link.MembershipChange
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return nil, err
		}
		cuts[linkCutSlot{seq: eventSeq, Member: event.Member}] = event.Scope
	}
	return cuts, rows.Err()
}

func readStagedLinkCuts(ctx context.Context, tx store.Tx, peer LinkEventPeer, expected map[linkCutSlot]link.MemberScope) ([]link.LeafCut, error) {
	rows, err := tx.QueryContext(ctx, `SELECT body FROM link_event_cut_stage WHERE device_id = ? AND stream = ? ORDER BY seq, vendor, key`,
		peer.Device, peer.Stream)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cuts := make([]link.LeafCut, 0, len(expected))
	for rows.Next() {
		var cut link.LeafCut
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(data), &cut); err != nil {
			return nil, err
		}
		slot := linkCutSlot{seq: cut.Seq, Member: cut.Member}
		if scope, found := expected[slot]; !found || scope != cut.Scope {
			return nil, errLinkCutScope
		}
		delete(expected, slot)
		cuts = append(cuts, cut)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(expected) != 0 {
		return nil, errLinkCutScope
	}
	return cuts, nil
}

func applyLinkCuts(ctx context.Context, tx store.Tx, peer LinkEventPeer, boot string, cuts []link.LeafCut) error {
	for _, cut := range cuts {
		res, err := tx.ExecContext(ctx, `INSERT INTO link_event_cut
			(device_id, vendor, key, boot, leaf_generation, binding_id, membership_generation, route_epoch, leaf_seq, absent, event_seq)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (device_id, vendor, key) DO UPDATE SET boot = excluded.boot,
				leaf_generation = excluded.leaf_generation, binding_id = excluded.binding_id,
				membership_generation = excluded.membership_generation, route_epoch = excluded.route_epoch,
				leaf_seq = excluded.leaf_seq, absent = excluded.absent, event_seq = excluded.event_seq
			WHERE excluded.event_seq >= link_event_cut.event_seq
				AND (link_event_cut.absent = 0 OR excluded.absent = 1 OR excluded.leaf_generation <> link_event_cut.leaf_generation)
				AND (excluded.boot <> link_event_cut.boot OR excluded.leaf_generation <> link_event_cut.leaf_generation
					OR excluded.absent = 1 OR excluded.leaf_seq >= link_event_cut.leaf_seq)`,
			peer.Device, cut.Vendor, cut.Key, boot, cut.Scope.LeafGeneration, cut.Scope.Membership.BindingID,
			cut.Scope.Membership.Generation, cut.Scope.RouteEpoch, cut.LeafSeq, cut.Absent, cut.Seq)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return errLinkCutScope
		}
		if cut.Absent {
			if err := markLinkOfferDirty(ctx, tx, peer.Device, cut.Member, ""); err != nil {
				return err
			}
		}
	}
	return nil
}

func finishLinkAck(ctx context.Context, tx store.Tx, peer LinkEventPeer, seq uint64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE link_event_cursor SET acked = ? WHERE device_id = ?`, seq, peer.Device); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM link_event_cut_stage WHERE device_id = ? AND stream = ?`, peer.Device, peer.Stream); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM link_event_snapshot WHERE device_id = ? AND stream = ?`, peer.Device, peer.Stream); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE link_event_stream SET ack_seq = NULL, snapshot_seq = NULL WHERE device_id = ? AND stream = ?`,
		peer.Device, peer.Stream); err != nil {
		return err
	}
	return trimAckedLinkEvents(ctx, tx, peer.Device, seq)
}

func trimAckedLinkEvents(ctx context.Context, tx store.Tx, device string, acked uint64) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM device_event WHERE device_id = ?1 AND seq IN (
		SELECT seq FROM device_event WHERE device_id = ?1 AND seq <= ?2 ORDER BY seq LIMIT 256)`, device, acked)
	return err
}
