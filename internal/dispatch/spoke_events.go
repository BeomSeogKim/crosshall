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

	"github.com/BeomSeogKim/crosshall/internal/domain"
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
	// ackBudget bounds the serialized cuts of one Ack frame, leaving room
	// for the frame's envelope under link.MaxFrame.
	ackBudget = link.MaxFrame - 64<<10
	// snapshotParts bounds the parts one snapshot may buffer.
	snapshotParts = 256
	// maxLeaseMS bounds a lease the home grants (it grants about a minute).
	maxLeaseMS = int64(time.Hour / time.Millisecond)
)

var errEventGap = errors.New("link: the event stream has a gap; a snapshot follows")

// errHomeRolledBack ends the event stream for good: the home sent a
// snapshot below what this device acknowledged, so it was restored to an
// older store. Nothing of it applies; the device links again (v1).
var errHomeRolledBack = errors.New("link: the home was restored to an older store; run crosshall link again")

// rolledBackFlag reads whether the spoke store recorded a home rollback.
// A store whose cursor was never written has none.
func rolledBackFlag(ctx context.Context, db *store.Store) (bool, error) {
	var flag bool
	err := db.View(ctx, func(tx store.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT rolled_back FROM spoke_event_cursor WHERE id = 1`).Scan(&flag)
	})
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return flag, err
}

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
	stop    *spokeMembers // halted on a home rollback
	// policy publishes (true) or withdraws (false) the hook policy files.
	policy func(ctx context.Context, publish bool) error
}

// newSpokeEvents starts the client of a dispatcher process, whose start
// already made the cache ineligible (invalidateEvents).
func newSpokeEvents(m *spokeMembers, policy func(context.Context, bool) error) (*spokeEvents, error) {
	boot, err := hostclock.Boot()
	if err != nil {
		return nil, err
	}
	return &spokeEvents{db: m.db, members: m.members, stop: m, boot: boot, mono: hostclock.Mono, policy: policy}, nil
}

// rolledBack stops the device: first in this process, so no command
// frame leaves after it, then in the store. A failed write still stops
// the device; persistStop is tried again until it holds.
func (e *spokeEvents) rolledBack(ctx context.Context, seq, acked uint64) error {
	e.stop.halted.Store(true)
	log.Printf("dispatcher: the home sent snapshot %d below acknowledged event %d: it was restored to an older store; the link stops until crosshall link runs again", seq, acked)
	// Recorded before waiting for the command turn, and also while the
	// dispatcher shuts down.
	record, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	recorded := e.persistStop(record)
	cancel()
	e.stop.halt(ctx)
	if recorded != nil {
		return fmt.Errorf("%w (recording it: %v)", errHomeRolledBack, recorded)
	}
	return errHomeRolledBack
}

// persistStop records the rollback, so a restarted dispatcher refuses too.
func (e *spokeEvents) persistStop(ctx context.Context) error {
	return e.db.Update(ctx, func(tx store.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE spoke_event_cursor SET eligible = 0, rolled_back = 1 WHERE id = 1`)
		return err
	})
}

// invalidateEvents records that nothing cached may be served until a
// snapshot applies. A dispatcher calls it before it is ready.
func invalidateEvents(ctx context.Context, db *store.Store) error {
	return db.Update(ctx, func(tx store.Tx) error {
		if err := suspendSpokeWakes(ctx, tx); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `UPDATE spoke_event_cursor SET eligible = 0 WHERE eligible = 1`)
		return err
	})
}

// ineligible stops serving the cache: the hook policy files go first, so
// hooks go quiet rather than act on a policy the device may have missed.
func (e *spokeEvents) ineligible(ctx context.Context) error {
	if e.policy != nil {
		if err := e.policy(ctx, false); err != nil {
			return err
		}
	}
	return invalidateEvents(ctx, e.db)
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

// linkRead is a frame with the monotonic time it was read at: a lease
// runs from its frame's arrival, not from when it was applied.
type linkRead struct {
	frame link.Frame
	at    int64
	err   error
}

type snapshotPart struct {
	link.Snapshot
	at int64
}

// run serves one event connection until it ends: subscribe, then apply
// and acknowledge what the home sends.
func (e *spokeEvents) run(ctx context.Context, conn spokeConn, allowWake bool) error {
	// The reader stops with this run, also when a frame it read is left
	// unconsumed.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if e.stop.halted.Load() {
		return errHomeRolledBack
	}
	var cur eventCursor
	snapshot := !e.resumed
	err := e.db.Update(ctx, func(tx store.Tx) error {
		// An acknowledged wake may have closed while disconnected. Only
		// fresh authorization on this stream can stage it again.
		if err := suspendSpokeWakes(ctx, tx); err != nil {
			return err
		}
		var err error
		if cur, err = e.cursor(ctx, tx); err != nil {
			return err
		}
		if cur.boot != e.boot || cur.acked == 0 || !cur.eligible {
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
	from := cur.acked
	if snapshot {
		// A snapshot needs no cursor, and a home restored to an older
		// store refuses one ahead of its head.
		e.resumed, from = false, 0
	}
	if err := e.send(conn, link.TypeSubscribe, link.Subscribe{FromSeq: from, Boot: e.boot, Snapshot: snapshot}); err != nil {
		return err
	}
	log.Printf("dispatcher: event stream subscribed from %d (snapshot %t)", from, snapshot)
	// A home that ends a resumed stream before its first frame may have
	// refused the cursor (a home restored to an older store): the next
	// subscribe asks for a snapshot.
	answered := false
	defer func() {
		if !answered && !snapshot {
			e.resumed = false
			_ = e.ineligible(ctx) // until a snapshot applies; run again invalidates if this failed
		}
	}()
	frames := make(chan linkRead, 1)
	go func() {
		for {
			f, err := conn.Receive(time.Time{})
			var at int64
			if err == nil {
				at, err = e.mono()
			}
			select {
			case frames <- linkRead{f, at, err}:
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
	var parts []snapshotPart
	var caughtUp bool
	var sendingWake int64
	// Hook facts and written acks go out once the home caught up, and
	// again on every connection: the home answers a repeated ID from what
	// it stored.
	hooksReady := false
	sent := map[int64]bool{}
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-e.stop.hooks:
			if hooksReady {
				if err := e.sendHooks(ctx, conn, sent); err != nil {
					return err
				}
			}
		case <-keepalive.C:
			if e.resumed && len(parts) == 0 {
				if err := e.send(conn, link.TypeAck, link.Ack{Seq: acked, Boot: e.boot}); err != nil {
					return err
				}
			}
		case r := <-frames:
			if r.err != nil {
				return r.err
			}
			answered = true
			switch r.frame.Type {
			case link.TypeSnapshot:
				var part link.Snapshot
				if err := json.Unmarshal(r.frame.Body, &part); err != nil {
					return fmt.Errorf("link: malformed snapshot: %w", err)
				}
				if part.Part != len(parts) || len(parts) > 0 && part.Seq != parts[0].Seq || len(parts) == snapshotParts {
					return errors.New("link: snapshot parts out of order")
				}
				if len(parts) == 0 && part.Seq < acked {
					return e.rolledBack(ctx, part.Seq, acked) // applies nothing, acknowledges nothing
				}
				if len(parts) == 0 {
					// The home replaces the cache: until the whole
					// snapshot applies, nothing cached is served.
					if err := e.ineligible(ctx); err != nil {
						return err
					}
					e.resumed = false
					caughtUp = false
				}
				parts = append(parts, snapshotPart{part, r.at})
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
				if !e.resumed || len(parts) > 0 {
					return errors.New("link: an event before the snapshot")
				}
				var ev link.Event
				if err := json.Unmarshal(r.frame.Body, &ev); err != nil {
					return fmt.Errorf("link: malformed event: %w", err)
				}
				if !allowWake && (ev.Kind == link.EventWake || ev.Kind == link.EventWakeClosed) {
					return errors.New("link: wake event without wake negotiation")
				}
				if ev.Seq <= acked {
					continue // already applied
				}
				if ev.Seq != acked+1 {
					e.resumed = false
					if err := e.ineligible(ctx); err != nil {
						return err
					}
					return errEventGap
				}
				cuts, err := e.applyEvent(ctx, ev, r.at)
				if err != nil {
					return err
				}
				acked = ev.Seq
				if err := e.ack(conn, acked, cuts); err != nil {
					return err
				}
			case link.TypeCaughtUp:
				if !hooksReady {
					hooksReady = true
					if err := e.sendHooks(ctx, conn, sent); err != nil {
						return err
					}
				}
				if !allowWake {
					continue
				}
				var ready link.CaughtUp
				if err := json.Unmarshal(r.frame.Body, &ready); err != nil {
					return fmt.Errorf("link: malformed catch-up: %w", err)
				}
				if !e.resumed || len(parts) != 0 || ready.Seq != acked {
					return errors.New("link: catch-up does not match the applied wake cursor")
				}
				caughtUp = true
				if sendingWake == 0 {
					var err error
					if sendingWake, err = e.sendWakeResult(ctx, conn); err != nil {
						return err
					}
				}
			case link.TypeWakeReplay:
				if !allowWake || !caughtUp {
					return errors.New("link: wake replay before wake negotiation and catch-up")
				}
				var wake link.Wake
				if err := json.Unmarshal(r.frame.Body, &wake); err != nil {
					return fmt.Errorf("link: malformed wake replay: %w", err)
				}
				if err := e.db.Update(ctx, func(tx store.Tx) error {
					return stageSpokeWake(ctx, tx, wake)
				}); err != nil {
					return err
				}
			case link.TypeWakeRecorded:
				if !allowWake {
					return errors.New("link: wake receipt without wake negotiation")
				}
				var receipt link.WakeRecorded
				if err := json.Unmarshal(r.frame.Body, &receipt); err != nil {
					return fmt.Errorf("link: malformed wake receipt: %w", err)
				}
				if err := e.db.Update(ctx, func(tx store.Tx) error {
					return recordSpokeWakeReceipt(ctx, tx, sendingWake, receipt)
				}); err != nil {
					return err
				}
				sendingWake = 0
				if caughtUp {
					var err error
					if sendingWake, err = e.sendWakeResult(ctx, conn); err != nil {
						return err
					}
				}
			case link.TypeRecorded:
				var rec link.Recorded
				if err := json.Unmarshal(r.frame.Body, &rec); err != nil {
					return fmt.Errorf("link: malformed recorded: %w", err)
				}
				// Applied, stale or quarantined: the home holds it now.
				if err := e.db.Update(ctx, func(tx store.Tx) error {
					_, err := tx.ExecContext(ctx, `DELETE FROM spoke_hook_outbox WHERE id = ?`, rec.ID)
					return err
				}); err != nil {
					return err
				}
				delete(sent, int64(rec.ID))
			default:
				return fmt.Errorf("link: unexpected %q frame on the event stream", link.Printable(r.frame.Type))
			}
		}
	}
}

func (e *spokeEvents) sendWakeResult(ctx context.Context, conn spokeConn) (int64, error) {
	var result *link.WakeResult
	if err := e.db.View(ctx, func(tx store.Tx) error {
		var err error
		result, err = nextSpokeWakeResult(ctx, tx)
		return err
	}); err != nil || result == nil {
		return 0, err
	}
	return result.AttemptID, e.send(conn, link.TypeWakeResult, result)
}

// sendHooks sends the queued hook frames this connection has not sent,
// each leaf's in the order of its publish sequence.
func (e *spokeEvents) sendHooks(ctx context.Context, conn spokeConn, sent map[int64]bool) error {
	type queued struct {
		id         int64
		kind, body string
	}
	var queue []queued
	err := e.db.View(ctx, func(tx store.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT id, kind, body FROM spoke_hook_outbox ORDER BY vendor, key, leaf_seq, id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var q queued
			if err := rows.Scan(&q.id, &q.kind, &q.body); err != nil {
				return err
			}
			if !sent[q.id] {
				queue = append(queue, q)
			}
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	for _, q := range queue {
		var err error
		switch q.kind {
		case "fact":
			var f link.Fact
			if err = json.Unmarshal([]byte(q.body), &f); err == nil {
				f.ID = uint64(q.id)
				err = e.send(conn, link.TypeFact, f)
			}
		default:
			var w link.Written
			if err = json.Unmarshal([]byte(q.body), &w); err == nil {
				w.ID = uint64(q.id)
				err = e.send(conn, link.TypeWritten, w)
			}
		}
		if err != nil {
			return err
		}
		sent[q.id] = true
	}
	return nil
}

func (e *spokeEvents) send(conn spokeConn, frame string, v any) error {
	if _, err := conn.Send(frame, v, time.Now().Add(eventWriteTimeout)); err != nil {
		conn.Close()
		return err
	}
	return nil
}

// ack acknowledges seq with its cuts, split into frames by their
// serialized size.
func (e *spokeEvents) ack(conn spokeConn, seq uint64, cuts []link.LeafCut) error {
	for {
		n, size := 0, 0
		for ; n < len(cuts); n++ {
			raw, err := json.Marshal(cuts[n])
			if err != nil {
				return err
			}
			if n > 0 && size+len(raw)+1 > ackBudget {
				break
			}
			size += len(raw) + 1
		}
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
	member                       link.Member
	scope                        link.MemberScope
	rendezvous, digest, identity string
	seq                          uint64
	dir                          *os.Root
	unlock                       func()
	absent                       bool
}

func (h *heldLeaf) release() {
	if h.unlock != nil {
		h.unlock()
		h.dir.Close()
	}
}

func (h *heldLeaf) cut(seq uint64) link.LeafCut {
	c := link.LeafCut{Member: h.member, Seq: seq, Scope: h.scope, Absent: h.absent}
	if !h.absent {
		c.LeafSeq = h.seq
	}
	return c
}

// hold takes the publish lock of the member's leaf when its registration
// is of the scope's generation and the directory at its path is the
// registered one; otherwise the change finds the leaf absent.
func (e *spokeEvents) hold(ctx context.Context, member link.Member, scope link.MemberScope) (*heldLeaf, error) {
	h := &heldLeaf{member: member, scope: scope, absent: true, rendezvous: e.members}
	var generation string
	err := e.db.View(ctx, func(tx store.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT digest, file_identity, generation FROM spoke_leaf WHERE vendor = ? AND key = ?`,
			member.Vendor, member.Key).Scan(&h.digest, &h.identity, &generation)
	})
	if errors.Is(err, sql.ErrNoRows) || err == nil && generation != scope.LeafGeneration {
		return h, nil
	}
	if err != nil {
		return nil, err
	}
	dir, err := ident.OpenSpokeLeaf(e.members, h.digest)
	if errors.Is(err, os.ErrNotExist) {
		return h, nil
	}
	if err != nil {
		return nil, err
	}
	if identity, err := ident.FileIdentity(dir); err != nil || identity != h.identity {
		dir.Close()
		return h, err
	}
	seq, unlock, err := spool.LockLeafPublish(ctx, dir)
	if err != nil {
		dir.Close()
		return nil, err
	}
	h.dir, h.seq, h.unlock, h.absent = dir, seq, unlock, false
	return h, nil
}

// confirm checks, in the applying transaction, that the held leaf is
// still the registration of the scope's generation: a registration that
// changed after hold turns the cut absent.
func confirm(ctx context.Context, tx store.Tx, h *heldLeaf) error {
	if h.absent {
		return nil
	}
	var generation, identity string
	err := tx.QueryRowContext(ctx, `SELECT generation, file_identity FROM spoke_leaf WHERE vendor = ? AND key = ?`,
		h.member.Vendor, h.member.Key).Scan(&generation, &identity)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err != nil || generation != h.scope.LeafGeneration || identity != h.identity {
		h.absent = true
		return nil
	}
	// The directory at the registered path, now that the lock is held:
	// a directory moved away from it is not this registration's leaf.
	live, err := ident.OpenSpokeLeaf(h.rendezvous, h.digest)
	if errors.Is(err, os.ErrNotExist) {
		h.absent = true
		return nil
	}
	if err != nil {
		return err
	}
	defer live.Close()
	if got, err := ident.FileIdentity(live); err != nil || got != h.identity {
		h.absent = true
		return err
	}
	return nil
}

// applyScope records a member's scope, and its membership on the held
// leaf when newer than the device's (as command answers do).
func applyScope(ctx context.Context, tx store.Tx, h *heldLeaf) error {
	if err := confirm(ctx, tx, h); err != nil {
		return err
	}
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
		WHERE digest = ? AND file_identity = ? AND membership_gen < ?`,
		s.Membership.BindingID, s.Membership.Generation, h.digest, h.identity, s.Membership.Generation)
	return err
}

// stamp republishes the held leaf's membership stamp before its lock is
// released.
func (e *spokeEvents) stamp(ctx context.Context, h *heldLeaf) error {
	if h.absent {
		return nil
	}
	return stampLocked(ctx, e.db, h.dir, h.digest, h.identity)
}

// applyEvent applies one event, read at monotonic time at, and advances
// the cursor in one transaction; a membership or invalidate event is
// applied under its leaf's publish lock and returns that leaf's cut.
func (e *spokeEvents) applyEvent(ctx context.Context, ev link.Event, at int64) ([]link.LeafCut, error) {
	var h *heldLeaf
	var offer link.Offer
	var policy link.AdapterPolicy
	switch ev.Kind {
	case link.EventWake, link.EventWakeClosed:
	case link.EventOffer:
		if err := json.Unmarshal(ev.Body, &offer); err != nil {
			return nil, fmt.Errorf("link: malformed offer: %w", err)
		}
	case link.EventPolicy:
		if err := json.Unmarshal(ev.Body, &policy); err != nil || !domain.ValidAdapterName(policy.Adapter) {
			return nil, fmt.Errorf("link: malformed policy: %v", err)
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
	err := e.db.Update(ctx, func(tx store.Tx) error {
		switch ev.Kind {
		case link.EventWake, link.EventWakeClosed:
			if err := stageSpokeWakeEvent(ctx, tx, ev); err != nil {
				return err
			}
		case link.EventOffer:
			if err := e.putOffer(ctx, tx, offer, at, false); err != nil {
				return err
			}
		case link.EventPolicy:
			if err := putPolicy(ctx, tx, policy); err != nil {
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
	if ev.Kind == link.EventPolicy {
		return nil, e.publishPolicy(ctx)
	}
	if h == nil {
		return nil, nil
	}
	if err := e.stamp(ctx, h); err != nil {
		// The store moved and the stamp did not: nothing cached counts
		// until a snapshot stamps it again.
		return nil, errors.Join(err, e.ineligible(ctx))
	}
	return []link.LeafCut{h.cut(ev.Seq)}, nil
}

func putPolicy(ctx context.Context, tx store.Tx, p link.AdapterPolicy) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO spoke_policy (adapter, enabled, reminder, epoch) VALUES (?, ?, ?, ?)
		ON CONFLICT (adapter) DO UPDATE SET enabled = excluded.enabled, reminder = excluded.reminder, epoch = excluded.epoch`,
		p.Adapter, p.Enabled, p.Reminder, p.Epoch)
	return err
}

// publishPolicy writes the hook policy files from the cached policies
// while the cache is eligible (#400, 12k).
func (e *spokeEvents) publishPolicy(ctx context.Context) error {
	if e.policy == nil {
		return nil
	}
	return e.policy(ctx, true)
}

// putOffer records an offer received at monotonic time at, under the
// lease rules of link.Offer. A snapshot keeps the earlier of an offer's
// two deadlines and its suppression; an event's newer lease epoch starts
// a fresh deadline.
func (e *spokeEvents) putOffer(ctx context.Context, tx store.Tx, o link.Offer, at int64, snapshot bool) error {
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
	next := at + min(o.LeaseMS, maxLeaseMS)*int64(time.Millisecond)
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
// returns a cut for every member. Each offer's lease runs from the time
// its part was read.
func (e *spokeEvents) applySnapshot(ctx context.Context, parts []snapshotPart) ([]link.LeafCut, error) {
	type member struct {
		link.SnapshotMember
		at int64
	}
	var members []member
	for _, p := range parts {
		for _, m := range p.Members {
			members = append(members, member{m, p.at})
		}
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
	seq := parts[0].Seq
	err := e.db.Update(ctx, func(tx store.Tx) error {
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
		if _, err := tx.ExecContext(ctx, `DELETE FROM spoke_policy`); err != nil {
			return err
		}
		for _, p := range parts[0].Policies {
			if !domain.ValidAdapterName(p.Adapter) {
				return errors.New("link: a snapshot names an invalid adapter")
			}
			if err := putPolicy(ctx, tx, p); err != nil {
				return err
			}
		}
		for i, m := range members {
			if err := applyScope(ctx, tx, held[i]); err != nil {
				return err
			}
			for _, o := range m.Offers {
				if err := e.putOffer(ctx, tx, o, m.at, true); err != nil {
					return err
				}
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO spoke_event_cursor (id, boot, acked_seq, eligible) VALUES (1, ?, ?, 0)
			ON CONFLICT (id) DO UPDATE SET boot = excluded.boot, acked_seq = excluded.acked_seq, eligible = 0`, e.boot, seq)
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
	// The cache counts only once every stamp is out, the leaves still held.
	err = e.db.Update(ctx, func(tx store.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE spoke_event_cursor SET eligible = 1 WHERE id = 1 AND acked_seq = ?`, seq)
		return err
	})
	if err != nil {
		return nil, err
	}
	return cuts, e.publishPolicy(ctx)
}
