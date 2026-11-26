package domain

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"slices"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/link"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

const (
	linkOfferLease    = time.Minute
	linkOfferPageSize = 64
)

func markLinkOfferDirty(ctx context.Context, tx store.Tx, device string, member link.Member, reason string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO device_offer_dirty (device_id, vendor, key, invalidate_reason)
		VALUES (?, ?, ?, ?)
		ON CONFLICT (device_id, vendor, key) DO UPDATE SET invalidate_reason =
			COALESCE(NULLIF(device_offer_dirty.invalidate_reason, ''), excluded.invalidate_reason)`,
		device, member.Vendor, member.Key, reason)
	return err
}

type linkOfferChange struct {
	link.Member
	reason     string
	membership bool
}

func refreshLinkEvents(ctx context.Context, tx store.Tx, device string, now time.Time) error {
	if _, err := tx.ExecContext(ctx, `INSERT INTO device_offer_dirty (device_id, vendor, key)
		SELECT device_id, vendor, key FROM device_offer WHERE device_id = ? AND expires_at_ms <= ?
		ORDER BY expires_at_ms LIMIT ?
		ON CONFLICT (device_id, vendor, key) DO NOTHING`, device, ms(now), linkOfferPageSize); err != nil {
		return err
	}
	changes, err := linkOfferChanges(ctx, tx, device)
	if err != nil {
		return err
	}
	for _, change := range changes {
		if err := refreshLinkOffer(ctx, tx, device, change, now); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM device_offer_dirty WHERE device_id = ? AND vendor = ? AND key = ?`,
			device, change.Vendor, change.Key); err != nil {
			return err
		}
	}
	return nil
}

func linkOfferChanges(ctx context.Context, tx store.Tx, device string) ([]linkOfferChange, error) {
	rows, err := tx.QueryContext(ctx, `SELECT vendor, key, invalidate_reason, membership
		FROM device_offer_dirty WHERE device_id = ? ORDER BY vendor, key LIMIT ?`, device, linkOfferPageSize)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var changes []linkOfferChange
	for rows.Next() {
		var change linkOfferChange
		if err := rows.Scan(&change.Vendor, &change.Key, &change.reason, &change.membership); err != nil {
			return nil, err
		}
		changes = append(changes, change)
	}
	return changes, rows.Err()
}

type linkOfferScope struct {
	link.MemberScope
	binding    hookBinding
	principal  int64
	credential int64
	current    bool
	absent     bool
}

func readLinkOfferScope(ctx context.Context, tx store.Tx, device string, member link.Member) (linkOfferScope, error) {
	var scope linkOfferScope
	err := tx.QueryRowContext(ctx, `SELECT k.principal_id, k.credential_gen, k.state = 'live' AND k.credential_gen = d.credential_gen,
		k.leaf_generation, COALESCE(b.id, 0), COALESCE(s.generation, 0), COALESCE(b.route_epoch, 0),
		COALESCE(b.channel_id, 0), COALESCE(b.state, ''), COALESCE(b.dnd, 0),
		EXISTS (SELECT 1 FROM link_event_cut c WHERE c.device_id = k.device_id AND c.vendor = k.vendor AND c.key = k.key
			AND c.absent = 1 AND c.leaf_generation = k.leaf_generation)
		FROM device_key k JOIN device d ON d.device_id = k.device_id
		JOIN principal p ON p.id = k.principal_id AND p.device_id = d.device_id
		LEFT JOIN member_selection s ON s.principal_id = p.id
		LEFT JOIN binding b ON b.id = s.binding_id AND b.principal_id = p.id
		WHERE k.device_id = ? AND k.vendor = ? AND k.key = ?`, device, member.Vendor, member.Key).
		Scan(&scope.principal, &scope.credential, &scope.current, &scope.LeafGeneration,
			&scope.Membership.BindingID, &scope.Membership.Generation, &scope.RouteEpoch,
			&scope.binding.channel, &scope.binding.state, &scope.binding.dnd, &scope.absent)
	if errors.Is(err, sql.ErrNoRows) {
		return scope, nil
	}
	scope.binding.id, scope.binding.routeEpoch = scope.Membership.BindingID, scope.RouteEpoch
	return scope, err
}

func (s linkOfferScope) live() bool {
	return s.current && s.binding.state == "live" && !s.absent
}

func refreshLinkOffer(ctx context.Context, tx store.Tx, device string, change linkOfferChange, now time.Time) error {
	scope, err := readLinkOfferScope(ctx, tx, device, change.Member)
	if err != nil {
		return err
	}
	old, expires, err := readPreparedLinkOffer(ctx, tx, device, change.Member)
	if err != nil {
		return err
	}
	next, profile, err := selectLinkOffer(ctx, tx, device, change.Member, scope)
	if err != nil {
		return err
	}
	reuse := change.reason == "" && old.OfferID != 0 && sameLinkOffer(old, next)
	if scope.Membership.BindingID == 0 {
		return clearPreparedLinkOffer(ctx, tx, device, change.Member)
	}
	if !reuse && (old.OfferID != 0 || change.reason != "") {
		reason := change.reason
		switch {
		case scope.binding.state == "departed":
			reason = link.InvalidateDepart
		case reason == "":
			reason = link.InvalidateOfferSet
		}
		if _, err := appendLinkEvent(ctx, tx, device, link.EventInvalidate, link.Invalidate{
			Member: change.Member, Reason: reason, Scope: scope.MemberScope,
		}, now); err != nil {
			return err
		}
		if err := clearPreparedLinkOffer(ctx, tx, device, change.Member); err != nil {
			return err
		}
	}
	if change.membership && scope.live() {
		if _, err := appendLinkEvent(ctx, tx, device, link.EventMembership, link.MembershipChange{
			Member: change.Member, Scope: scope.MemberScope,
		}, now); err != nil {
			return err
		}
	}
	if reuse {
		old.LeaseMS = expires - ms(now)
		if old.LeaseMS <= 0 {
			old.LeaseEpoch++
			old.LeaseMS = linkOfferLease.Milliseconds()
			if err := renewLinkOffer(ctx, tx, device, old, now); err != nil {
				return err
			}
		}
		// A snapshot may have omitted this member's still-dirty offer.
		_, err := appendLinkEvent(ctx, tx, device, link.EventOffer, old, now)
		return err
	}
	if len(next.IDs) == 0 {
		return nil
	}
	return prepareLinkOffer(ctx, tx, device, scope, next, profile, now)
}

func selectLinkOffer(ctx context.Context, tx store.Tx, device string, member link.Member, scope linkOfferScope) (link.Offer, wire.HookProfile, error) {
	var next link.Offer
	var profile wire.HookProfile
	if !scope.live() || scope.binding.dnd {
		return next, profile, nil
	}
	var raw []byte
	err := tx.QueryRowContext(ctx, `SELECT l.launch_ref, l.policy_epoch, p.epoch, a.profile
		FROM link_event_launch l
		JOIN adapter_launch a ON a.principal_id = l.principal_id AND a.launch_ref = l.projection_ref
		JOIN adapter_policy p ON p.adapter = ? AND p.enabled = 1 AND p.epoch = l.policy_epoch
		WHERE l.principal_id = ? AND l.device_id = ? AND l.credential_gen = ? AND l.leaf_generation = ?
			AND l.binding_id = ? AND l.membership_generation = ? AND l.route_epoch = ? AND a.ended_hook_at_ms IS NULL`,
		member.Vendor, scope.principal, device, scope.credential, scope.LeafGeneration, scope.Membership.BindingID,
		scope.Membership.Generation, scope.RouteEpoch).Scan(&next.Launch, &next.Epoch, &next.PolicyEpoch, &raw)
	if errors.Is(err, sql.ErrNoRows) {
		return next, profile, nil
	}
	if err != nil {
		return next, profile, err
	}
	if err := json.Unmarshal(raw, &profile); err != nil {
		return next, profile, err
	}
	next.Member, next.LeafGeneration, next.Membership = member, scope.LeafGeneration, scope.Membership
	next.BindingID, next.RouteEpoch = scope.Membership.BindingID, scope.RouteEpoch
	next.IDs, err = pendingOfferIDs(ctx, tx, scope.principal, scope.binding.channel, true)
	next.IDs = next.IDs[:min(len(next.IDs), OfferCap)]
	return next, profile, err
}

func readPreparedLinkOffer(ctx context.Context, tx store.Tx, device string, member link.Member) (link.Offer, int64, error) {
	var offer link.Offer
	var raw []byte
	var expires int64
	err := tx.QueryRowContext(ctx, `SELECT body, expires_at_ms FROM device_offer WHERE device_id = ? AND vendor = ? AND key = ?`,
		device, member.Vendor, member.Key).Scan(&raw, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return offer, 0, nil
	}
	if err != nil {
		return offer, 0, err
	}
	err = json.Unmarshal(raw, &offer)
	return offer, expires, err
}

func sameLinkOffer(a, b link.Offer) bool {
	return a.Member == b.Member && a.LeafGeneration == b.LeafGeneration && a.Membership == b.Membership &&
		a.RouteEpoch == b.RouteEpoch && a.PolicyEpoch == b.PolicyEpoch && a.Launch == b.Launch &&
		a.Epoch == b.Epoch && slices.Equal(a.IDs, b.IDs)
}

func clearPreparedLinkOffer(ctx context.Context, tx store.Tx, device string, member link.Member) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM device_offer WHERE device_id = ? AND vendor = ? AND key = ?`,
		device, member.Vendor, member.Key)
	return err
}

func renewLinkOffer(ctx context.Context, tx store.Tx, device string, offer link.Offer, now time.Time) error {
	raw, err := json.Marshal(offer)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE device_offer SET expires_at_ms = ?, body = ?
		WHERE device_id = ? AND vendor = ? AND key = ?`,
		ms(now)+offer.LeaseMS, string(raw), device, offer.Vendor, offer.Key)
	return err
}

func prepareLinkOffer(ctx context.Context, tx store.Tx, device string, scope linkOfferScope, offer link.Offer, profile wire.HookProfile, now time.Time) error {
	offer.Nonce = rand.Text()
	offer.LeaseMS = linkOfferLease.Milliseconds()
	id, err := recordHookOffer(ctx, tx, scope.principal, scope.binding, profile, offer.Launch, "prompt", offer.LeafGeneration,
		uint64(offer.Epoch), "provisional", &offer.Nonce, offer.IDs, now)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE offer SET device_credential_gen = ?, device_membership_gen = ? WHERE id = ?`,
		scope.credential, scope.Membership.Generation, id); err != nil {
		return err
	}
	offer.OfferID = id
	raw, err := json.Marshal(offer)
	if err != nil {
		return err
	}
	if len(raw) > link.MaxFrame/2 {
		return errors.New("domain: linked offer exceeds the event size limit")
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO device_offer (device_id, vendor, key, offer_id, expires_at_ms, body)
		VALUES (?, ?, ?, ?, ?, ?)`, device, offer.Vendor, offer.Key, offer.OfferID, ms(now)+offer.LeaseMS, string(raw)); err != nil {
		return err
	}
	_, err = appendLinkEvent(ctx, tx, device, link.EventOffer, offer, now)
	return err
}

func snapshotLinkMembers(ctx context.Context, tx store.Tx, device string, now time.Time) ([]link.SnapshotMember, error) {
	rows, err := tx.QueryContext(ctx, `SELECT k.vendor, k.key, k.leaf_generation, b.id, s.generation, b.route_epoch,
			o.body, o.expires_at_ms
		FROM device_key k JOIN device d ON d.device_id = k.device_id AND d.credential_gen = k.credential_gen
		JOIN principal p ON p.id = k.principal_id AND p.device_id = d.device_id
		JOIN member_selection s ON s.principal_id = p.id
		JOIN binding b ON b.id = s.binding_id AND b.principal_id = p.id AND b.state = 'live'
		LEFT JOIN device_offer o ON o.device_id = k.device_id AND o.vendor = k.vendor AND o.key = k.key
			AND o.expires_at_ms > ? AND b.dnd = 0
			AND NOT EXISTS (SELECT 1 FROM device_offer_dirty pending
				WHERE pending.device_id = k.device_id AND pending.vendor = k.vendor AND pending.key = k.key)
			AND EXISTS (
				SELECT 1 FROM link_event_launch l
				JOIN adapter_launch a ON a.principal_id = l.principal_id AND a.launch_ref = l.projection_ref
				JOIN adapter_policy policy ON policy.adapter = k.vendor AND policy.enabled = 1 AND policy.epoch = l.policy_epoch
				WHERE l.principal_id = p.id AND l.device_id = k.device_id AND l.credential_gen = k.credential_gen
					AND l.leaf_generation = k.leaf_generation AND l.binding_id = b.id
					AND l.membership_generation = s.generation AND l.route_epoch = b.route_epoch
					AND a.ended_hook_at_ms IS NULL AND l.launch_ref = json_extract(o.body, '$.launch')
					AND l.policy_epoch = json_extract(o.body, '$.policy_epoch'))
		WHERE k.device_id = ? AND k.state = 'live' AND NOT EXISTS (
			SELECT 1 FROM link_event_cut c WHERE c.device_id = k.device_id AND c.vendor = k.vendor AND c.key = k.key
				AND c.absent = 1 AND c.leaf_generation = k.leaf_generation)
		ORDER BY k.vendor, k.key`, ms(now), device)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var members []link.SnapshotMember
	for rows.Next() {
		var member link.SnapshotMember
		var raw []byte
		var expires sql.NullInt64
		if err := rows.Scan(&member.Vendor, &member.Key, &member.Scope.LeafGeneration, &member.Scope.Membership.BindingID,
			&member.Scope.Membership.Generation, &member.Scope.RouteEpoch, &raw, &expires); err != nil {
			return nil, err
		}
		if expires.Valid {
			var offer link.Offer
			if err := json.Unmarshal(raw, &offer); err != nil {
				return nil, err
			}
			if offer.LeafGeneration == member.Scope.LeafGeneration && offer.Membership == member.Scope.Membership &&
				offer.RouteEpoch == member.Scope.RouteEpoch {
				offer.LeaseMS = expires.Int64 - ms(now)
				member.Offers = []link.Offer{offer}
			}
		}
		members = append(members, member)
	}
	return members, rows.Err()
}
