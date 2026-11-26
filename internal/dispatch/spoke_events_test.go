package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/link"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// fakeHome is the home end of an event connection.
type fakeHome struct {
	mu     sync.Mutex
	sent   []link.Frame
	in     chan link.Frame
	closed bool
}

func newFakeHome() *fakeHome { return &fakeHome{in: make(chan link.Frame, 16)} }

func (h *fakeHome) Send(frame string, v any, _ time.Time) (bool, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return false, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sent = append(h.sent, link.Frame{Type: frame, Body: body})
	return true, nil
}

func (h *fakeHome) Receive(time.Time) (link.Frame, error) {
	f, ok := <-h.in
	if !ok {
		return link.Frame{}, errors.New("link ended")
	}
	return f, nil
}

func (h *fakeHome) Close() {
	h.mu.Lock()
	h.closed = true
	h.mu.Unlock()
}

func (h *fakeHome) push(t *testing.T, typ string, v any) {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	h.in <- link.Frame{Type: typ, Body: body}
}

func (h *fakeHome) frames() []link.Frame {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]link.Frame(nil), h.sent...)
}

// ack waits for the acknowledgement of seq and returns its cuts.
func (h *fakeHome) ack(t *testing.T, seq uint64) []link.LeafCut {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var cuts []link.LeafCut
		found := false
		for _, f := range h.frames() {
			var a link.Ack
			if f.Type == link.TypeAck && json.Unmarshal(f.Body, &a) == nil && a.Seq == seq {
				cuts, found = append(cuts, a.Cuts...), !a.More
			}
		}
		if found {
			return cuts
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("no ack of %d in %v", seq, h.frames())
	return nil
}

type eventFixture struct {
	*spokeFixture
	e    *spokeEvents
	now  int64
	done chan error
}

func newEventFixture(t *testing.T) *eventFixture {
	t.Helper()
	f := &eventFixture{spokeFixture: newSpokeFixture(t), now: int64(time.Hour)}
	f.e = &spokeEvents{db: f.m.db, members: f.m.members, boot: "boot-1", mono: func() (int64, error) { return f.now, nil }}
	if err := f.dir.WriteFile("hook-publish-seq", []byte("5\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *eventFixture) start(t *testing.T, home *fakeHome) {
	t.Helper()
	f.done = make(chan error, 1)
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	go func() { f.done <- f.e.run(ctx, home) }()
}

func (f *eventFixture) end(t *testing.T, home *fakeHome) error {
	t.Helper()
	close(home.in)
	select {
	case err := <-f.done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the event client did not stop")
		return nil
	}
}

func (f *eventFixture) scope(binding, gen int64) link.MemberScope {
	return link.MemberScope{LeafGeneration: f.generation, Membership: link.Membership{BindingID: binding, Generation: gen}, RouteEpoch: 1}
}

var eventMember = link.Member{Vendor: "codex", Key: "session"}

func offer(id int64, epoch uint64, leaseMS int64) link.Offer {
	return link.Offer{Member: eventMember, OfferID: id, Nonce: "n", IDs: []int64{1}, LeaseMS: leaseMS, LeaseEpoch: epoch}
}

func event(t *testing.T, seq uint64, kind string, v any) link.Event {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return link.Event{Seq: seq, Kind: kind, Body: body}
}

// A snapshot in parts replaces the cache and is acknowledged with a cut
// for every member; events then apply in order, each acknowledged after
// it committed, and a gap ends the stream for a snapshot.
func TestASpokeKeepsTheEventStream(t *testing.T) {
	f := newEventFixture(t)
	home := newFakeHome()
	f.start(t, home)
	stranger := link.Member{Vendor: "claude", Key: "elsewhere"}
	home.push(t, link.TypeSnapshot, link.Snapshot{Seq: 10, Part: 0, Members: []link.SnapshotMember{
		{Member: eventMember, Scope: f.scope(7, 3), Offers: []link.Offer{offer(1, 0, 60_000)}}}})
	home.push(t, link.TypeSnapshot, link.Snapshot{Seq: 10, Part: 1, Last: true, Members: []link.SnapshotMember{
		{Member: stranger, Scope: link.MemberScope{LeafGeneration: "g9", Membership: link.Membership{BindingID: 2, Generation: 1}}}}})
	cuts := home.ack(t, 10)
	var sub link.Subscribe
	if first := home.frames()[0]; first.Type != link.TypeSubscribe || json.Unmarshal(first.Body, &sub) != nil || !sub.Snapshot || sub.Boot != "boot-1" {
		t.Fatalf("first frame %+v", first)
	}
	if len(cuts) != 2 || cuts[0].Member != stranger || !cuts[0].Absent || cuts[1].Member != eventMember ||
		cuts[1].LeafSeq != 5 || cuts[1].Absent || cuts[1].Scope != f.scope(7, 3) {
		t.Fatalf("snapshot cuts %+v", cuts)
	}
	if n := f.count(t, `SELECT acked_seq * 10 + eligible FROM spoke_event_cursor`); n != 101 {
		t.Fatalf("cursor %d", n)
	}
	if m, err := ident.LookupMembership(f.dir); err != nil || m == nil || *m != (core.Membership{BindingID: 7, Generation: 3}) {
		t.Fatalf("leaf stamp %+v, %v", m, err)
	}
	home.push(t, link.TypeCaughtUp, link.CaughtUp{Seq: 10})
	// The same lease epoch keeps the earlier deadline; a newer one renews.
	f.now += int64(10 * time.Second)
	home.push(t, link.TypeEvent, event(t, 11, link.EventOffer, offer(1, 0, 60_000)))
	home.ack(t, 11)
	if n := f.count(t, `SELECT deadline_mono / 1000000000 FROM spoke_offer`); n != 3660 {
		t.Fatalf("same-epoch deadline %d s", n)
	}
	home.push(t, link.TypeEvent, event(t, 12, link.EventOffer, offer(1, 1, 60_000)))
	home.ack(t, 12)
	if n := f.count(t, `SELECT deadline_mono / 1000000000 FROM spoke_offer`); n != 3670 {
		t.Fatalf("renewed deadline %d s", n)
	}
	// A move retires the member's offers and moves its selection.
	home.push(t, link.TypeEvent, event(t, 13, link.EventInvalidate, link.Invalidate{Member: eventMember, Reason: link.InvalidateMove, Scope: f.scope(8, 4)}))
	if cuts := home.ack(t, 13); len(cuts) != 1 || cuts[0].LeafSeq != 5 || cuts[0].Seq != 13 || cuts[0].Scope != f.scope(8, 4) {
		t.Fatalf("move cut %+v", cuts)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM spoke_offer`); n != 0 {
		t.Fatalf("%d offers after the move", n)
	}
	if n := f.count(t, `SELECT binding_id * 10 + membership_gen FROM spoke_leaf`); n != 84 {
		t.Fatalf("selection %d after the move", n)
	}
	// An older membership never rolls the selection back.
	home.push(t, link.TypeEvent, event(t, 14, link.EventMembership, link.MembershipChange{Member: eventMember, Scope: f.scope(7, 3)}))
	home.ack(t, 14)
	if n := f.count(t, `SELECT binding_id * 10 + membership_gen FROM spoke_leaf`); n != 84 {
		t.Fatalf("selection %d after an older membership", n)
	}
	home.push(t, link.TypeEvent, event(t, 16, link.EventOffer, offer(2, 0, 60_000)))
	if err := f.end(t, home); !errors.Is(err, errEventGap) || f.e.resumed {
		t.Fatalf("a gap: %v, resumed %v", err, f.e.resumed)
	}
	if n := f.count(t, `SELECT acked_seq FROM spoke_event_cursor`); n != 14 {
		t.Fatalf("cursor %d after the gap", n)
	}
}

// An older lease epoch is ignored; a lease of zero retires the offer.
func TestASpokeKeepsOfferLeases(t *testing.T) {
	f := newEventFixture(t)
	put := func(o link.Offer) int {
		t.Helper()
		if err := f.m.db.Update(t.Context(), func(tx store.Tx) error { return f.e.putOffer(t.Context(), tx, o, f.now, false) }); err != nil {
			t.Fatal(err)
		}
		return f.count(t, `SELECT COALESCE(MAX(lease_epoch * 100000 + deadline_mono / 1000000000), -1) FROM spoke_offer`)
	}
	if got := put(offer(1, 2, 60_000)); got != 2*100000+3660 {
		t.Fatalf("first lease %d", got)
	}
	f.now += int64(time.Minute)
	if got := put(offer(1, 1, 60_000)); got != 2*100000+3660 {
		t.Fatalf("an older epoch moved the lease: %d", got)
	}
	if got := put(offer(1, 2, 0)); got != -1 {
		t.Fatalf("a zero lease kept the offer: %d", got)
	}
}

// A snapshot that ends before its last part changes nothing, and parts
// out of order end the stream.
func TestASpokeAppliesOnlyAWholeSnapshot(t *testing.T) {
	f := newEventFixture(t)
	home := newFakeHome()
	f.start(t, home)
	home.push(t, link.TypeSnapshot, link.Snapshot{Seq: 10, Part: 0, Members: []link.SnapshotMember{
		{Member: eventMember, Scope: f.scope(7, 3), Offers: []link.Offer{offer(1, 0, 60_000)}}}})
	if err := f.end(t, home); err == nil {
		t.Fatal("the stream ended cleanly mid-snapshot")
	}
	if n := f.count(t, `SELECT COUNT(*) FROM spoke_offer`) + f.count(t, `SELECT eligible FROM spoke_event_cursor`); n != 0 {
		t.Fatalf("a partial snapshot applied: %d", n)
	}
	home = newFakeHome()
	f.start(t, home)
	home.push(t, link.TypeSnapshot, link.Snapshot{Seq: 10, Part: 1, Last: true})
	if err := f.end(t, home); err == nil || errors.Is(err, errEventGap) {
		t.Fatalf("parts out of order: %v", err)
	}
	// A member listed twice ends the stream instead of waiting for itself.
	home = newFakeHome()
	f.start(t, home)
	twice := link.SnapshotMember{Member: eventMember, Scope: f.scope(7, 3)}
	home.push(t, link.TypeSnapshot, link.Snapshot{Seq: 10, Last: true, Members: []link.SnapshotMember{twice, twice}})
	select {
	case err := <-f.done:
		if err == nil {
			t.Fatal("a duplicate member applied")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a duplicate member hung the stream")
	}
	if n := f.count(t, `SELECT COUNT(*) FROM spoke_member_scope`); n != 0 {
		t.Fatalf("a refused snapshot applied %d scopes", n)
	}
}

// A reconnect in the same process resumes from the cursor; a new process
// or another boot asks for a snapshot.
func TestASpokeResumesOnlyWithinItsProcessAndBoot(t *testing.T) {
	f := newEventFixture(t)
	home := newFakeHome()
	f.start(t, home)
	home.push(t, link.TypeSnapshot, link.Snapshot{Seq: 10, Last: true})
	home.ack(t, 10)
	f.end(t, home)
	subscribe := func() link.Subscribe {
		t.Helper()
		home := newFakeHome()
		f.start(t, home)
		deadline := time.Now().Add(5 * time.Second)
		for len(home.frames()) == 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		f.end(t, home)
		var s link.Subscribe
		if err := json.Unmarshal(home.frames()[0].Body, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if s := subscribe(); s.Snapshot || s.FromSeq != 10 {
		t.Fatalf("reconnect %+v", s)
	}
	f.e = &spokeEvents{db: f.m.db, members: f.m.members, boot: "boot-1", mono: f.e.mono}
	if s := subscribe(); !s.Snapshot {
		t.Fatalf("new process %+v", s)
	}
	if n := f.count(t, `SELECT eligible FROM spoke_event_cursor`); n != 0 {
		t.Fatal("the cache stayed eligible while a snapshot was due")
	}
	f.e.resumed = true
	f.e.boot = "boot-2"
	if s := subscribe(); !s.Snapshot || s.Boot != "boot-2" {
		t.Fatalf("new boot %+v", s)
	}
}

// A change to a member applies under its leaf's publish lock: a hook that
// holds the lock delays it, and the cut names the sequence after that
// hook's publication.
func TestASpokeCutsALeafUnderItsPublishLock(t *testing.T) {
	f := newEventFixture(t)
	home := newFakeHome()
	f.start(t, home)
	home.push(t, link.TypeSnapshot, link.Snapshot{Seq: 10, Last: true, Members: []link.SnapshotMember{{Member: eventMember, Scope: f.scope(7, 3)}}})
	home.ack(t, 10)
	_, unlock, err := spool.LockLeafPublish(t.Context(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	home.push(t, link.TypeEvent, event(t, 11, link.EventMembership, link.MembershipChange{Member: eventMember, Scope: f.scope(8, 4)}))
	time.Sleep(50 * time.Millisecond)
	for _, fr := range home.frames() {
		var a link.Ack
		if fr.Type == link.TypeAck && json.Unmarshal(fr.Body, &a) == nil && a.Seq == 11 {
			t.Fatal("the change applied while a hook held the leaf")
		}
	}
	if err := f.dir.WriteFile("hook-publish-seq", []byte("6\n"), 0600); err != nil {
		t.Fatal(err)
	}
	unlock()
	if cuts := home.ack(t, 11); len(cuts) != 1 || cuts[0].LeafSeq != 6 {
		t.Fatalf("cut %+v", cuts)
	}
	// While a change commits, the leaf stays locked: a hook waits for it.
	paused, resume := make(chan struct{}), make(chan struct{})
	f.e.mono = func() (int64, error) {
		close(paused)
		<-resume
		return f.now, nil
	}
	home.push(t, link.TypeEvent, event(t, 12, link.EventMembership, link.MembershipChange{Member: eventMember, Scope: f.scope(9, 5)}))
	<-paused
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, unlock, err := spool.LockLeafPublish(ctx, f.dir); err == nil {
		unlock()
		t.Fatal("a hook took the leaf while a change was applying")
	}
	close(resume)
	home.ack(t, 12)
	if _, unlock, err := spool.LockLeafPublish(t.Context(), f.dir); err != nil {
		t.Fatalf("the leaf stayed locked: %v", err)
	} else {
		unlock()
	}
	f.end(t, home)
}
