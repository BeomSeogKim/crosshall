package domain

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/link"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// Leave space for the owner-spool result envelope as well as the frame.
const linkEventPageBytes = link.MaxFrame - 4096

type linkEventCursor struct {
	head, acked, trimmed uint64
	boot                 string
	credentialGen        int64
}

type linkEventStream struct {
	boot                string
	wakeCapable         bool
	snapshotSeq, ackSeq sql.NullInt64
}

func readLinkEventCursor(ctx context.Context, tx store.Tx, device string) (linkEventCursor, error) {
	var cursor linkEventCursor
	err := tx.QueryRowContext(ctx, `SELECT head, acked, trimmed, boot, credential_gen FROM link_event_cursor WHERE device_id = ?`, device).
		Scan(&cursor.head, &cursor.acked, &cursor.trimmed, &cursor.boot, &cursor.credentialGen)
	return cursor, err
}

func readLinkEventStream(ctx context.Context, tx store.Tx, peer LinkEventPeer) (linkEventStream, error) {
	var stream linkEventStream
	err := tx.QueryRowContext(ctx, `SELECT boot, snapshot_seq, ack_seq, wake_capable FROM link_event_stream
		WHERE device_id = ? AND stream = ? AND admission_gen = ? AND closed_at_ms IS NULL`,
		peer.Device, peer.Stream, peer.Admission).Scan(&stream.boot, &stream.snapshotSeq, &stream.ackSeq, &stream.wakeCapable)
	return stream, err
}

func appendLinkEvent(ctx context.Context, tx store.Tx, device, kind string, payload any, now time.Time) (uint64, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	if len(data)+128 > linkEventPageBytes {
		return 0, errors.New("link event exceeds the page budget")
	}
	var seq uint64
	err = tx.QueryRowContext(ctx, `INSERT INTO link_event_cursor (device_id, head) VALUES (?, 1)
		ON CONFLICT (device_id) DO UPDATE SET head = head + 1 RETURNING head`, device).Scan(&seq)
	if err != nil {
		return 0, err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO device_event (device_id, seq, kind, body, at_ms) VALUES (?, ?, ?, ?, ?)`,
		device, seq, kind, string(data), ms(now))
	return seq, err
}

func refuseLinkEvents(message string) core.Result {
	return Refuse("link_event_resync", message, "reconnect and subscribe to the link events again")
}

func (s *Service) linkEvents(ctx context.Context, tx store.Tx, caller core.Caller, cmd LinkEvents, now time.Time) (core.Result, error) {
	if refusal, err := authorizeLinkEventPeer(ctx, tx, caller, cmd.LinkEventPeer); err != nil || refusal != nil {
		if refusal != nil {
			return *refusal, nil
		}
		return core.Result{}, err
	}
	if cmd.Close {
		if cmd.Subscribe != nil || cmd.SnapshotPart != nil || cmd.FromSeq != 0 || cmd.Resubscribe || cmd.AllowWake || cmd.AllowPolicy {
			return refuseLinkEvents("invalid event stream close"), nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE link_event_stream SET closed_at_ms = ?
			WHERE device_id = ? AND stream = ? AND admission_gen = ? AND closed_at_ms IS NULL`,
			ms(now), cmd.Device, cmd.Stream, cmd.Admission); err != nil {
			return core.Result{}, err
		}
		return OK(LinkEventsResult{})
	}
	if cmd.Subscribe != nil {
		if cmd.SnapshotPart != nil || cmd.FromSeq != 0 || cmd.Subscribe.Boot == "" || !validRef(cmd.Subscribe.Boot) {
			return refuseLinkEvents("invalid event subscription"), nil
		}
		if cmd.Resubscribe {
			if _, err := readLinkEventStream(ctx, tx, cmd.LinkEventPeer); errors.Is(err, sql.ErrNoRows) {
				return refuseLinkEvents("this link no longer owns the active subscription"), nil
			} else if err != nil {
				return core.Result{}, err
			}
		}
		return startLinkEventStream(ctx, tx, cmd.LinkEventPeer, *cmd.Subscribe, cmd.AllowWake, cmd.AllowPolicy, now)
	}
	stream, err := readLinkEventStream(ctx, tx, cmd.LinkEventPeer)
	if errors.Is(err, sql.ErrNoRows) {
		return refuseLinkEvents("events require an active subscription"), nil
	}
	if err != nil {
		return core.Result{}, err
	}
	if cmd.SnapshotPart != nil {
		if *cmd.SnapshotPart < 0 || cmd.FromSeq != 0 {
			return refuseLinkEvents("invalid snapshot page"), nil
		}
		return linkSnapshotPage(ctx, tx, cmd.LinkEventPeer, *cmd.SnapshotPart, now)
	}
	if err := refreshLinkEvents(ctx, tx, cmd.Device, stream.boot, now); err != nil {
		return core.Result{}, err
	}
	cursor, err := readLinkEventCursor(ctx, tx, cmd.Device)
	if err != nil {
		return core.Result{}, err
	}
	if cmd.FromSeq < cursor.trimmed || cmd.FromSeq > cursor.head {
		return refuseLinkEvents("the event cursor is outside the retained stream"), nil
	}
	if cmd.FromSeq >= cursor.acked && cursor.acked > cursor.trimmed {
		if err := trimAckedLinkEvents(ctx, tx, cmd.Device, cursor.acked); err != nil {
			return core.Result{}, err
		}
	}
	return linkEventPage(ctx, tx, cmd.Device, cmd.FromSeq, cursor.head, now)
}

func startLinkEventStream(ctx context.Context, tx store.Tx, peer LinkEventPeer, sub link.Subscribe, allowWake, allowPolicy bool, now time.Time) (core.Result, error) {
	if _, err := tx.ExecContext(ctx, `INSERT INTO link_event_cursor (device_id) VALUES (?) ON CONFLICT DO NOTHING`, peer.Device); err != nil {
		return core.Result{}, err
	}
	if err := refreshLinkEvents(ctx, tx, peer.Device, sub.Boot, now); err != nil {
		return core.Result{}, err
	}
	cursor, err := readLinkEventCursor(ctx, tx, peer.Device)
	if err != nil {
		return core.Result{}, err
	}
	if sub.FromSeq > cursor.head {
		return refuseLinkEvents("the event cursor is ahead of the home"), nil
	}
	// A stream that gains the policy capability missed the policy changes
	// made while its device had none.
	var hadPolicy bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM link_event_stream WHERE device_id = ? AND policy_capable = 1)`,
		peer.Device).Scan(&hadPolicy); err != nil {
		return core.Result{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM link_event_stream WHERE device_id = ?`, peer.Device); err != nil {
		return core.Result{}, err
	}
	snapshotNeeded := sub.Snapshot || sub.FromSeq == 0 || sub.FromSeq < cursor.trimmed ||
		sub.FromSeq > cursor.acked || sub.Boot != cursor.boot || peer.CredentialGen != cursor.credentialGen ||
		allowPolicy && !hadPolicy
	// Events a stream without their capability may not see are replaced
	// by a snapshot.
	for _, gated := range []struct {
		allowed bool
		kind    string
	}{{allowWake, link.EventWake}, {allowPolicy, link.EventPolicy}} {
		if gated.allowed || snapshotNeeded {
			continue
		}
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM device_event
			WHERE device_id = ? AND seq > ? AND kind = ?)`, peer.Device, sub.FromSeq, gated.kind).Scan(&snapshotNeeded); err != nil {
			return core.Result{}, err
		}
	}
	var snapshotSeq *uint64
	if snapshotNeeded {
		snapshotSeq = &cursor.head
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO link_event_stream (device_id, stream, admission_gen, boot, snapshot_seq, created_at_ms,
		wake_capable, policy_capable) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, peer.Device, peer.Stream, peer.Admission, sub.Boot, snapshotSeq,
		ms(now), allowWake, allowPolicy); err != nil {
		return core.Result{}, err
	}
	if !snapshotNeeded {
		return linkEventPage(ctx, tx, peer.Device, sub.FromSeq, cursor.head, now)
	}
	if err := prepareLinkSnapshot(ctx, tx, peer, sub.Boot, cursor.head, allowPolicy, now); err != nil {
		return core.Result{}, err
	}
	return linkSnapshotPage(ctx, tx, peer, 0, now)
}

func linkEventPage(ctx context.Context, tx store.Tx, device string, from, head uint64, now time.Time) (core.Result, error) {
	rows, err := tx.QueryContext(ctx, `SELECT seq, kind, body, at_ms FROM device_event WHERE device_id = ? AND seq > ?
		ORDER BY seq LIMIT 128`, device, from)
	if err != nil {
		return core.Result{}, err
	}
	defer rows.Close()
	page := LinkEventsResult{Through: head}
	size := 64
	for rows.Next() {
		var event link.Event
		var body string
		var at int64
		if err := rows.Scan(&event.Seq, &event.Kind, &body, &at); err != nil {
			return core.Result{}, err
		}
		if event.Seq != from+uint64(len(page.Events))+1 {
			return refuseLinkEvents("the retained event stream has a gap"), nil
		}
		event.Body = json.RawMessage(body)
		if event.Kind == link.EventOffer {
			var offer link.Offer
			if err := json.Unmarshal(event.Body, &offer); err != nil {
				return core.Result{}, err
			}
			offer.LeaseMS = remainingLinkLease(offer.LeaseMS, at, now)
			event.Body, err = json.Marshal(offer)
			if err != nil {
				return core.Result{}, err
			}
		}
		if event.Kind == link.EventWake {
			if err := checkLinkWakeEvent(ctx, tx, device, &event); err != nil {
				return core.Result{}, err
			}
		}
		size += len(event.Body) + 128
		if size > linkEventPageBytes {
			break
		}
		page.Events = append(page.Events, event)
	}
	if err := rows.Err(); err != nil {
		return core.Result{}, err
	}
	if len(page.Events) == 0 && from < head {
		return refuseLinkEvents("the next link event does not fit a page"), nil
	}
	return OK(page)
}

func prepareLinkSnapshot(ctx context.Context, tx store.Tx, peer LinkEventPeer, boot string, seq uint64, allowPolicy bool, now time.Time) error {
	members, err := snapshotLinkMembers(ctx, tx, peer.Device, boot, now)
	if err != nil {
		return err
	}
	var policies []link.AdapterPolicy
	if allowPolicy {
		if policies, err = linkAdapterPolicies(ctx, tx, ""); err != nil {
			return err
		}
	}
	snapshot := link.Snapshot{Seq: seq, Members: []link.SnapshotMember{}, Policies: policies}
	save := func(last bool) error {
		snapshot.Last = last
		data, err := json.Marshal(snapshot)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO link_event_snapshot (device_id, stream, part, body) VALUES (?, ?, ?, ?)`,
			peer.Device, peer.Stream, snapshot.Part, string(data))
		return err
	}
	size := 128
	for _, member := range members {
		data, err := json.Marshal(member)
		if err != nil {
			return err
		}
		if len(data)+129 > linkEventPageBytes {
			return fmt.Errorf("snapshot member %q is too large", member.Vendor)
		}
		if size+len(data)+1 > linkEventPageBytes {
			if err := save(false); err != nil {
				return err
			}
			snapshot.Part++
			snapshot.Members, snapshot.Policies = snapshot.Members[:0], nil
			size = 128
		}
		snapshot.Members = append(snapshot.Members, member)
		size += len(data) + 1
	}
	return save(true)
}

func linkSnapshotPage(ctx context.Context, tx store.Tx, peer LinkEventPeer, part int, now time.Time) (core.Result, error) {
	var data string
	var at int64
	err := tx.QueryRowContext(ctx, `SELECT p.body, s.created_at_ms FROM link_event_snapshot p JOIN link_event_stream s
		USING (device_id, stream) WHERE p.device_id = ? AND p.stream = ? AND part = ?`, peer.Device, peer.Stream, part).Scan(&data, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return refuseLinkEvents("that snapshot page is no longer available"), nil
	}
	if err != nil {
		return core.Result{}, err
	}
	var snapshot link.Snapshot
	if err := json.Unmarshal([]byte(data), &snapshot); err != nil {
		return core.Result{}, err
	}
	for i := range snapshot.Members {
		for j := range snapshot.Members[i].Offers {
			offer := &snapshot.Members[i].Offers[j]
			offer.LeaseMS = remainingLinkLease(offer.LeaseMS, at, now)
		}
	}
	return OK(LinkEventsResult{Snapshot: &snapshot, Through: snapshot.Seq})
}

func remainingLinkLease(lease, at int64, now time.Time) int64 {
	return max(0, lease-max(0, ms(now)-at))
}

func validLinkInteger(n uint64) bool { return n <= math.MaxInt64 }

// linkAdapterPolicies reads the hall's adapter policies as a linked
// device's hooks check them: one adapter, or every one with a policy.
func linkAdapterPolicies(ctx context.Context, tx store.Tx, adapter string) ([]link.AdapterPolicy, error) {
	rows, err := tx.QueryContext(ctx, `SELECT adapter, enabled, reminder, epoch FROM adapter_policy
		WHERE ? = '' OR adapter = ? ORDER BY adapter`, adapter, adapter)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []link.AdapterPolicy
	for rows.Next() {
		var p link.AdapterPolicy
		if err := rows.Scan(&p.Adapter, &p.Enabled, &p.Reminder, &p.Epoch); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// appendLinkPolicy sends an adapter's new policy to every linked device
// with an event stream, in order with the offers that carry its epoch.
// A device without a stream yet gets it in its first snapshot.
func appendLinkPolicy(ctx context.Context, tx store.Tx, adapter string, now time.Time) error {
	policies, err := linkAdapterPolicies(ctx, tx, adapter)
	if err != nil || len(policies) != 1 {
		return err
	}
	// Only a device whose last stream negotiated the capability: a device
	// that subscribes with it after one that had none takes a snapshot.
	rows, err := tx.QueryContext(ctx, `SELECT c.device_id FROM link_event_cursor c
		JOIN link_event_stream s ON s.device_id = c.device_id AND s.policy_capable = 1 ORDER BY c.device_id`)
	if err != nil {
		return err
	}
	var devices []string
	for rows.Next() {
		var device string
		if err := rows.Scan(&device); err != nil {
			rows.Close()
			return err
		}
		devices = append(devices, device)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, device := range devices {
		if _, err := appendLinkEvent(ctx, tx, device, link.EventPolicy, policies[0], now); err != nil {
			return err
		}
	}
	return nil
}
