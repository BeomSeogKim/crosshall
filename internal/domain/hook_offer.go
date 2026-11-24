package domain

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/notice"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// A capable hook may retry every eligible ID until its output receipt arrives.
// Legacy hooks count their offers on commit, including offers from before upgrade.
func (req Request) offerPending(ctx context.Context, tx store.Tx, principal int64, b hookBinding, p wire.HookProfile, launch, reason string,
	out *wire.HookResult, now time.Time) error {
	if b.dnd {
		return nil // held: no offer, nothing spent, no acknowledgement
	}
	capable := req.Adapter != nil && req.Adapter.OfferReceipts
	ids, err := selectIDs(ctx, tx, `SELECT r.message_id FROM recipient r JOIN message m ON m.id = r.message_id
		WHERE r.principal_id = ? AND m.channel_id = ? AND r.picked_up_at_ms IS NULL AND `+notMoreTime+`
			AND `+noClosedObligation+`
			AND NOT EXISTS (SELECT 1 FROM offer_item oi JOIN offer o ON o.id = oi.offer_id
				WHERE oi.principal_id = r.principal_id AND oi.message_id = r.message_id
					AND (o.written_at_ms IS NOT NULL OR o.receipt_mode = 'legacy' OR (o.receipt_mode = 'historical' AND NOT ?)))
			AND NOT EXISTS (SELECT 1 FROM announcement a WHERE a.principal_id = r.principal_id AND a.message_id = r.message_id
				AND a.via = 'turn_end')
			AND NOT EXISTS (SELECT 1 FROM publish_attempt_item i JOIN publish_attempt pa ON pa.id = i.attempt_id
				WHERE i.principal_id = r.principal_id AND i.message_id = r.message_id
					AND pa.observation IN ('own_turn', 'absorbed', 'unknown'))
		ORDER BY r.message_id LIMIT ?`, principal, b.channel, capable, OfferCap+1)
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
	batch := notice.IDList(ids)
	out.PendingLine = notice.PendingPrefix + batch

	var generation string
	if req.Adapter != nil {
		generation = req.Adapter.Generation
	}
	mode := "legacy"
	var nonce *string
	if capable {
		if generation == "" {
			return errors.New("domain: hook offer without authenticated generation")
		}
		receipt := &wire.HookOfferReceipt{}
		err := tx.QueryRowContext(ctx, `SELECT o.id, o.nonce FROM offer o
			JOIN offer_item first ON first.offer_id = o.id AND first.principal_id = o.principal_id AND first.message_id = ?
			WHERE o.principal_id = ? AND o.binding_id = ? AND o.route_epoch = ? AND o.generation = ?
				AND o.launch_ref = ? AND o.epoch = ? AND o.receipt_mode = 'provisional' AND o.written_at_ms IS NULL
				AND (SELECT group_concat(message_id, ' ' ORDER BY message_id) FROM offer_item WHERE offer_id = o.id) = ?
			ORDER BY o.id DESC LIMIT 1`, ids[0], principal, b.id, b.routeEpoch, generation, launch, out.Epoch, batch).
			Scan(&receipt.OfferID, &receipt.Nonce)
		if err == nil {
			out.OfferID, out.OfferReceipt = receipt.OfferID, receipt
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		receipt.Nonce = rand.Text()
		out.OfferReceipt = receipt
		mode, nonce = "provisional", &receipt.Nonce
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO offer
		(principal_id, binding_id, route_epoch, generation, launch_ref, epoch, reason, evidence, at_ms, receipt_mode, nonce)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, principal, b.id, b.routeEpoch, generation, launch, out.Epoch, reason,
		evidence(p), ms(now), mode, nonce)
	if err != nil {
		return err
	}
	if out.OfferID, err = res.LastInsertId(); err != nil {
		return err
	}
	if out.OfferReceipt != nil {
		out.OfferReceipt.OfferID = out.OfferID
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `INSERT INTO offer_item (offer_id, message_id, principal_id) VALUES (?, ?, ?)`,
			out.OfferID, id, principal); err != nil {
			return err
		}
	}
	return nil
}

// ConfirmHookOffer records the earliest reported write for this exact batch. An
// enqueued receipt can arrive after other offers, pickup or a dispatcher restart.
func ConfirmHookOffer(ctx context.Context, tx store.Tx, principal int64, generation string, receipt wire.HookOfferReceipt, writtenAtMS int64) error {
	if writtenAtMS <= 0 || receipt.OfferID <= 0 || receipt.Nonce == "" || generation == "" {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE offer SET written_at_ms = ?1
		WHERE id = ?2 AND nonce = ?3 AND principal_id = ?4 AND generation = ?5
			AND receipt_mode = 'provisional' AND (written_at_ms IS NULL OR written_at_ms > ?1)
			AND EXISTS (SELECT 1 FROM leaf WHERE principal_id = ?4 AND generation = ?5 AND state = 'live')
			AND EXISTS (SELECT 1 FROM binding b WHERE b.id = offer.binding_id AND b.principal_id = ?4
				AND b.route_epoch = offer.route_epoch AND b.state = 'live')`,
		writtenAtMS, receipt.OfferID, receipt.Nonce, principal, generation)
	return err
}
