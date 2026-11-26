package dispatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/hostclock"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/link"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

const (
	// eventKeepalive repeats the acknowledged sequence on an idle stream,
	// well within the home's retention of a stream from its last ack.
	eventKeepalive = time.Hour
	// eventWriteTimeout bounds one frame to a home that stopped reading.
	eventWriteTimeout = 30 * time.Second
	// cutsPerAck keeps an Ack frame far below link.MaxFrame.
	cutsPerAck = 1000
	// snapshotParts bounds the parts one snapshot may buffer.
	snapshotParts = 256
	// maxLeaseMS bounds a lease the home grants (it grants about a minute).
	maxLeaseMS = int64(time.Hour / time.Millisecond)
)

var errEventGap = errors.New("link: the event stream has a gap; a snapshot follows")

// spokeEvents keeps the home's event stream in the spoke store (#400,
// design 12j): each member's scope and the offers the home prepared, with
// the stream's cursor. Every event applies in one transaction that also
// advances the cursor, and its Ack leaves only after that commit.
type spokeEvents struct {
	db      *store.Store
	members string // the spoke's member rendezvous
	boot    string
	// resumed is set once this process applied a snapshot: until then a
	// subscribe asks for one.
	resumed bool
	mono    func() (int64, error)
}

func newSpokeEvents(db *store.Store, members string) (*spokeEvents, error) {
	boot, err := hostclock.Boot()
	if err != nil {
		return nil, err
	}
	return &spokeEvents{db: db, members: members, boot: boot, mono: hostclock.Mono}, nil
}

type eventCursor struct {
	boot     string
	acked    uint64
	eligible bool
}

func (e *spokeEvents) cursor(ctx context.Context, tx store.Tx) (eventCursor, error) {
	var c eventCursor
	err := tx.QueryRowContext(ctx, `SELECT boot, acked_seq, eligible FROM spoke_event_cursor WHERE id = 1`).
		Scan(&c.boot, &c.acked, &c.eligible)
	if errors.Is(err, sql.ErrNoRows) {
		return eventCursor{}, nil
	}
	return c, err
}

// run serves one event connection until it ends: subscribe, then apply
// and acknowledge what the home sends.
func (e *spokeEvents) run(ctx context.Context, conn spokeConn) error {
	// The reader stops with this run, also when a frame it read is left
	// unconsumed.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var cur eventCursor
	snapshot := !e.resumed
	err := e.db.Update(ctx, func(tx store.Tx) error {
		var err error
		if cur, err = e.cursor(ctx, tx); err != nil {
			return err
		}
		if cur.boot != e.boot || cur.acked == 0 {
			snapshot = true
		}
		if !snapshot {
			return nil
		}
		// From now until a snapshot applies, nothing cached is served.
		_, err = tx.ExecContext(ctx, `INSERT INTO spoke_event_cursor (id, boot, acked_seq, eligible) VALUES (1, ?, ?, 0)
			ON CONFLICT (id) DO UPDATE SET eligible = 0`, cur.boot, cur.acked)
		return err
	})
	if err != nil {
		return err
	}
	if err := e.send(conn, link.TypeSubscribe, link.Subscribe{FromSeq: cur.acked, Boot: e.boot, Snapshot: snapshot}); err != nil {
		return err
	}
	log.Printf("dispatcher: event stream subscribed from %d (snapshot %t)", cur.acked, snapshot)
	frames := make(chan linkRead, 1)
	go func() {
		for {
			f, err := conn.Receive(time.Time{})
			select {
			case frames <- linkRead{f, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	keepalive := time.NewTicker(eventKeepalive)
	defer keepalive.Stop()
	acked := cur.acked
	var parts []link.Snapshot
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-keepalive.C:
			if e.resumed {
				if err := e.send(conn, link.TypeAck, link.Ack{Seq: acked, Boot: e.boot}); err != nil {
					return err
				}
			}
		case r := <-frames:
			if r.err != nil {
				return r.err
			}
			switch r.frame.Type {
			case link.TypeSnapshot:
				var part link.Snapshot
				if err := json.Unmarshal(r.frame.Body, &part); err != nil {
					return fmt.Errorf("link: malformed snapshot: %w", err)
				}
				if part.Part != len(parts) || len(parts) > 0 && part.Seq != parts[0].Seq || len(parts) == snapshotParts {
					return errors.New("link: snapshot parts out of order")
				}
				parts = append(parts, part)
				if !part.Last {
					continue
				}
				cuts, err := e.applySnapshot(ctx, parts)
				if err != nil {
					return err
				}
				acked, parts, e.resumed = part.Seq, nil, true
				if err := e.ack(conn, acked, cuts); err != nil {
					return err
				}
			case link.TypeEvent:
				if !e.resumed {
					return errors.New("link: an event before the snapshot")
				}
				var ev link.Event
				if err := json.Unmarshal(r.frame.Body, &ev); err != nil {
					return fmt.Errorf("link: malformed event: %w", err)
				}
				if ev.Seq <= acked {
					continue // already applied
				}
				if ev.Seq != acked+1 {
					e.resumed = false
					return errEventGap
				}
				cuts, err := e.applyEvent(ctx, ev)
				if err != nil {
					return err
				}
				acked = ev.Seq
				if err := e.ack(conn, acked, cuts); err != nil {
					return err
				}
			case link.TypeCaughtUp, link.TypeRecorded:
				// Nothing is forwarded on this stream yet (6s-b).
			default:
				return fmt.Errorf("link: unexpected %q frame on the event stream", link.Printable(r.frame.Type))
			}
		}
	}
}

type linkRead struct {
	frame link.Frame
	err   error
}

func (e *spokeEvents) send(conn spokeConn, frame string, v any) error {
	if _, err := conn.Send(frame, v, time.Now().Add(eventWriteTimeout)); err != nil {
		conn.Close()
		return err
	}
	return nil
}

// ack acknowledges seq with its cuts, in frames of at most cutsPerAck.
func (e *spokeEvents) ack(conn spokeConn, seq uint64, cuts []link.LeafCut) error {
	for {
		n := min(len(cuts), cutsPerAck)
		a := link.Ack{Seq: seq, Boot: e.boot, Cuts: cuts[:n], More: n < len(cuts)}
		if err := e.send(conn, link.TypeAck, a); err != nil {
			return err
		}
		if cuts = cuts[n:]; len(cuts) == 0 {
			return nil
		}
	}
}

// heldLeaf is a member's leaf held under its publish lock while a change
// to it commits.
type heldLeaf struct {
	member link.Member
	scope  link.MemberScope
	seq    uint64
	dir    *os.Root
	unlock func()
	absent bool
}

func (h *heldLeaf) release() {
	if h.unlock != nil {
		h.unlock()
		h.dir.Close()
	}
}

func (h *heldLeaf) cut(seq uint64) link.LeafCut {
	return link.LeafCut{Member: h.member, Seq: seq, LeafSeq: h.seq, Scope: h.scope, Absent: h.absent}
}

// hold takes the publish lock of the member's leaf when its current
// generation is the scope's; otherwise the change finds it absent.
func (e *spokeEvents) hold(ctx context.Context, member link.Member, scope link.MemberScope) (*heldLeaf, error) {
	h := &heldLeaf{member: member, scope: scope}
	var digest, generation string
	err := e.db.View(ctx, func(tx store.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT digest, generation FROM spoke_leaf WHERE vendor = ? AND key = ?`,
			member.Vendor, member.Key).Scan(&digest, &generation)
	})
	if errors.Is(err, sql.ErrNoRows) || err == nil && generation != scope.LeafGeneration {
		h.absent = true
		return h, nil
	}
	if err != nil {
		return nil, err
	}
	if h.dir, err = ident.OpenSpokeLeaf(e.members, digest); errors.Is(err, os.ErrNotExist) {
		h.absent = true
		return h, nil
	} else if err != nil {
		return nil, err
	}
	if h.seq, h.unlock, err = spool.LockLeafPublish(ctx, h.dir); err != nil {
		h.dir.Close()
		return nil, err
	}
	return h, nil
}

// applyScope records a member's scope, and its membership on the leaf
// when newer than the device's (as command answers do).
func applyScope(ctx context.Context, tx store.Tx, h *heldLeaf) error {
	s := h.scope
	if _, err := tx.ExecContext(ctx, `INSERT INTO spoke_member_scope (vendor, key, leaf_generation, binding_id, membership_gen, route_epoch)
		VALUES (?, ?, ?, ?, ?, ?) ON CONFLICT (vendor, key) DO UPDATE SET leaf_generation = excluded.leaf_generation,
			binding_id = excluded.binding_id, membership_gen = excluded.membership_gen, route_epoch = excluded.route_epoch`,
		h.member.Vendor, h.member.Key, s.LeafGeneration, s.Membership.BindingID, s.Membership.Generation, s.RouteEpoch); err != nil {
		return err
	}
	if h.absent || s.Membership.BindingID == 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `UPDATE spoke_leaf SET binding_id = ?, membership_gen = ?
		WHERE vendor = ? AND key = ? AND generation = ? AND membership_gen < ?`,
		s.Membership.BindingID, s.Membership.Generation, h.member.Vendor, h.member.Key, s.LeafGeneration, s.Membership.Generation)
	return err
}

// stamp republishes the leaf's membership stamp from the store, before
// its lock is released.
func (e *spokeEvents) stamp(ctx context.Context, h *heldLeaf) error {
	if h.absent {
		return nil
	}
	var m core.Membership
	err := e.db.View(ctx, func(tx store.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT binding_id, membership_gen FROM spoke_leaf WHERE vendor = ? AND key = ?`,
			h.member.Vendor, h.member.Key).Scan(&m.BindingID, &m.Generation)
	})
	if err != nil || m.Generation == 0 {
		return err
	}
	return ident.RepublishMembership(h.dir, m)
}

// applyEvent applies one event and advances the cursor in one
// transaction; a membership or invalidate event is applied under its
// leaf's publish lock and returns that leaf's cut.
func (e *spokeEvents) applyEvent(ctx context.Context, ev link.Event) ([]link.LeafCut, error) {
	var h *heldLeaf
	var offer link.Offer
	switch ev.Kind {
	case link.EventOffer:
		if err := json.Unmarshal(ev.Body, &offer); err != nil {
			return nil, fmt.Errorf("link: malformed offer: %w", err)
		}
	case link.EventInvalidate, link.EventMembership:
		var change link.Invalidate
		if err := json.Unmarshal(ev.Body, &change); err != nil {
			return nil, fmt.Errorf("link: malformed %s: %w", ev.Kind, err)
		}
		var err error
		if h, err = e.hold(ctx, change.Member, change.Scope); err != nil {
			return nil, err
		}
		defer h.release()
	default:
		return nil, fmt.Errorf("link: unknown event kind %q", link.Printable(ev.Kind))
	}
	now, err := e.mono()
	if err != nil {
		return nil, err
	}
	err = e.db.Update(ctx, func(tx store.Tx) error {
		switch {
		case ev.Kind == link.EventOffer:
			if err := e.putOffer(ctx, tx, offer, now, false); err != nil {
				return err
			}
		default:
			if ev.Kind == link.EventInvalidate {
				if _, err := tx.ExecContext(ctx, `DELETE FROM spoke_offer WHERE vendor = ? AND key = ?`,
					h.member.Vendor, h.member.Key); err != nil {
					return err
				}
			}
			if err := applyScope(ctx, tx, h); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, `UPDATE spoke_event_cursor SET acked_seq = ? WHERE id = 1 AND acked_seq = ?`, ev.Seq, ev.Seq-1)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			return fmt.Errorf("link: the event cursor moved under event %d: %v", ev.Seq, err)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if h == nil {
		return nil, nil
	}
	if err := e.stamp(ctx, h); err != nil {
		return nil, err
	}
	return []link.LeafCut{h.cut(ev.Seq)}, nil
}

// putOffer records an offer under the lease rules of link.Offer. A
// snapshot keeps the earlier of an offer's two deadlines and its
// suppression; an event's newer lease epoch starts a fresh deadline.
func (e *spokeEvents) putOffer(ctx context.Context, tx store.Tx, o link.Offer, now int64, snapshot bool) error {
	var epoch uint64
	var deadline int64
	var boot string
	err := tx.QueryRowContext(ctx, `SELECT lease_epoch, deadline_mono, boot FROM spoke_offer
		WHERE vendor = ? AND key = ? AND offer_id = ?`, o.Vendor, o.Key, o.OfferID).Scan(&epoch, &deadline, &boot)
	held := err == nil && boot == e.boot
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if held && o.LeaseEpoch < epoch {
		return nil // an older lease
	}
	if o.LeaseMS <= 0 {
		_, err := tx.ExecContext(ctx, `DELETE FROM spoke_offer WHERE vendor = ? AND key = ? AND offer_id = ?`, o.Vendor, o.Key, o.OfferID)
		return err
	}
	next := now + min(o.LeaseMS, maxLeaseMS)*int64(time.Millisecond)
	if held && (snapshot || o.LeaseEpoch == epoch) {
		next = min(next, deadline)
	}
	body, err := json.Marshal(o)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO spoke_offer (vendor, key, offer_id, body, lease_epoch, deadline_mono, boot)
		VALUES (?, ?, ?, ?, ?, ?, ?) ON CONFLICT (vendor, key, offer_id) DO UPDATE SET body = excluded.body,
			lease_epoch = excluded.lease_epoch, deadline_mono = excluded.deadline_mono, boot = excluded.boot`,
		o.Vendor, o.Key, o.OfferID, string(body), max(o.LeaseEpoch, epoch), next, e.boot)
	return err
}

// applySnapshot replaces the cache with the snapshot's members and offers
// in one transaction, every present leaf held under its publish lock, and
// returns a cut for every member.
func (e *spokeEvents) applySnapshot(ctx context.Context, parts []link.Snapshot) ([]link.LeafCut, error) {
	var members []link.SnapshotMember
	for _, p := range parts {
		members = append(members, p.Members...)
	}
	// Leaves are locked in one order; a hook holds only its own. A member
	// listed twice would wait for its own lock.
	sort.Slice(members, func(i, j int) bool {
		if members[i].Vendor != members[j].Vendor {
			return members[i].Vendor < members[j].Vendor
		}
		return members[i].Key < members[j].Key
	})
	for i := 1; i < len(members); i++ {
		if members[i].Member == members[i-1].Member {
			return nil, errors.New("link: a snapshot lists a member twice")
		}
	}
	held := make([]*heldLeaf, 0, len(members))
	defer func() {
		for _, h := range held {
			h.release()
		}
	}()
	for _, m := range members {
		h, err := e.hold(ctx, m.Member, m.Scope)
		if err != nil {
			return nil, err
		}
		held = append(held, h)
	}
	now, err := e.mono()
	if err != nil {
		return nil, err
	}
	seq := parts[0].Seq
	err = e.db.Update(ctx, func(tx store.Tx) error {
		// Offers the snapshot no longer lists are retired; one it lists
		// keeps its earlier deadline and its suppression.
		type offerKey struct {
			link.Member
			id int64
		}
		listed := map[offerKey]bool{}
		for _, m := range members {
			for _, o := range m.Offers {
				if o.Member != m.Member {
					return errors.New("link: a snapshot offer names another member")
				}
				listed[offerKey{o.Member, o.OfferID}] = true
			}
		}
		rows, err := tx.QueryContext(ctx, `SELECT vendor, key, offer_id, boot FROM spoke_offer`)
		if err != nil {
			return err
		}
		var retired []offerKey
		for rows.Next() {
			var k offerKey
			var boot string
			if err := rows.Scan(&k.Vendor, &k.Key, &k.id, &boot); err != nil {
				rows.Close()
				return err
			}
			if boot != e.boot || !listed[k] {
				retired = append(retired, k)
			}
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, k := range retired {
			if _, err := tx.ExecContext(ctx, `DELETE FROM spoke_offer WHERE vendor = ? AND key = ? AND offer_id = ?`,
				k.Vendor, k.Key, k.id); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM spoke_member_scope`); err != nil {
			return err
		}
		for i, m := range members {
			if err := applyScope(ctx, tx, held[i]); err != nil {
				return err
			}
			for _, o := range m.Offers {
				if err := e.putOffer(ctx, tx, o, now, true); err != nil {
					return err
				}
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO spoke_event_cursor (id, boot, acked_seq, eligible) VALUES (1, ?, ?, 1)
			ON CONFLICT (id) DO UPDATE SET boot = excluded.boot, acked_seq = excluded.acked_seq, eligible = 1`, e.boot, seq)
		return err
	})
	if err != nil {
		return nil, err
	}
	cuts := make([]link.LeafCut, 0, len(held))
	for _, h := range held {
		if err := e.stamp(ctx, h); err != nil {
			return nil, err
		}
		cuts = append(cuts, h.cut(seq))
	}
	return cuts, nil
}
