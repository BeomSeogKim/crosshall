package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/link"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
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
	if err := link.NewWriter(io.Discard).Write(frame, v); err != nil {
		return false, err // the link's own limits
	}
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
	now  atomic.Int64
	done chan error
}

func newEventFixture(t *testing.T) *eventFixture {
	t.Helper()
	f := &eventFixture{spokeFixture: newSpokeFixture(t)}
	f.now.Store(int64(time.Hour))
	f.e = &spokeEvents{db: f.m.db, members: f.m.members, boot: "boot-1", mono: func() (int64, error) { return f.now.Load(), nil }}
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
	f.now.Add(int64(10 * time.Second))
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
		if err := f.m.db.Update(t.Context(), func(tx store.Tx) error { return f.e.putOffer(t.Context(), tx, o, f.now.Load(), false) }); err != nil {
			t.Fatal(err)
		}
		return f.count(t, `SELECT COALESCE(MAX(lease_epoch * 100000 + deadline_mono / 1000000000), -1) FROM spoke_offer`)
	}
	if got := put(offer(1, 2, 60_000)); got != 2*100000+3660 {
		t.Fatalf("first lease %d", got)
	}
	f.now.Add(int64(time.Minute))
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
	// Another writer holds the store, so the change waits to commit.
	writing, done := make(chan struct{}), make(chan struct{})
	go f.m.db.Update(t.Context(), func(store.Tx) error {
		close(writing)
		<-done
		return nil
	})
	<-writing
	home.push(t, link.TypeEvent, event(t, 12, link.EventMembership, link.MembershipChange{Member: eventMember, Scope: f.scope(9, 5)}))
	time.Sleep(50 * time.Millisecond) // the client holds the leaf, waiting for the store
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if _, unlock, err := spool.LockLeafPublish(ctx, f.dir); err == nil {
		unlock()
		t.Fatal("a hook took the leaf while a change was applying")
	}
	close(done)
	home.ack(t, 12)
	if _, unlock, err := spool.LockLeafPublish(t.Context(), f.dir); err != nil {
		t.Fatalf("the leaf stayed locked: %v", err)
	} else {
		unlock()
	}
	f.end(t, home)
}

// A directory that replaced the registered one at its path, with the
// store still naming the old one, is cut absent and keeps no new stamp.
func TestASpokeCutsAReplacedLeafAbsent(t *testing.T) {
	f := newEventFixture(t)
	if err := os.Rename(f.path, f.path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.path, "hook-publish-seq"), []byte("99\n"), 0600); err != nil {
		t.Fatal(err)
	}
	home := newFakeHome()
	f.start(t, home)
	home.push(t, link.TypeSnapshot, link.Snapshot{Seq: 10, Last: true, Members: []link.SnapshotMember{{Member: eventMember, Scope: f.scope(7, 3)}}})
	if cuts := home.ack(t, 10); len(cuts) != 1 || !cuts[0].Absent || cuts[0].LeafSeq != 0 {
		t.Fatalf("cut %+v", cuts)
	}
	if n := f.count(t, `SELECT membership_gen FROM spoke_leaf`); n != 0 {
		t.Fatalf("a replaced leaf took membership generation %d", n)
	}
	if _, err := os.Stat(filepath.Join(f.path, "membership")); !os.IsNotExist(err) {
		t.Fatalf("the replacement got a stamp: %v", err)
	}
	f.end(t, home)
}

// A stamp is written under the leaf's lock from the store as it is then,
// never from an older read.
func TestASpokeStampsALeafUnderItsLock(t *testing.T) {
	f := newEventFixture(t)
	if err := f.m.db.Update(t.Context(), func(tx store.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE spoke_leaf SET binding_id = 8, membership_gen = 4`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_, unlock, err := spool.LockLeafPublish(t.Context(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if err := stampLeaf(ctx, f.m.db, f.m.members, "codex", "session"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a stamp while a hook held the leaf: %v", err)
	}
	unlock()
	if err := stampLeaf(t.Context(), f.m.db, f.m.members, "codex", "session"); err != nil {
		t.Fatal(err)
	}
	if m, err := ident.LookupMembership(f.dir); err != nil || m == nil || *m != (core.Membership{BindingID: 8, Generation: 4}) {
		t.Fatalf("stamp %+v, %v", m, err)
	}
}

// The cache stops counting the moment it is known stale: a new process, a
// gap, the first part of a snapshot the home sends unasked; and while it
// does not count, every subscribe asks for a snapshot.
func TestASpokeCacheIsIneligibleUntilASnapshotApplies(t *testing.T) {
	f := newEventFixture(t)
	home := newFakeHome()
	f.start(t, home)
	home.push(t, link.TypeSnapshot, link.Snapshot{Seq: 10, Last: true})
	home.ack(t, 10)
	eligible := func() int { return f.count(t, `SELECT eligible FROM spoke_event_cursor`) }
	// A gap.
	home.push(t, link.TypeEvent, event(t, 12, link.EventOffer, offer(1, 0, 60_000)))
	if err := f.end(t, home); !errors.Is(err, errEventGap) || eligible() != 0 {
		t.Fatalf("after a gap: %v, eligible %d", err, eligible())
	}
	home = newFakeHome()
	f.start(t, home)
	home.push(t, link.TypeSnapshot, link.Snapshot{Seq: 12, Last: true})
	home.ack(t, 12)
	// A snapshot the home sends on a resumed stream, cut off midway.
	home.push(t, link.TypeSnapshot, link.Snapshot{Seq: 20, Part: 0})
	f.end(t, home)
	if eligible() != 0 {
		t.Fatal("a cache stayed eligible through a cut-off snapshot")
	}
	home = newFakeHome()
	f.start(t, home)
	for len(home.frames()) == 0 {
		time.Sleep(time.Millisecond)
	}
	var sub link.Subscribe
	if err := json.Unmarshal(home.frames()[0].Body, &sub); err != nil || !sub.Snapshot {
		t.Fatalf("subscribe while ineligible %+v, %v", sub, err)
	}
	home.push(t, link.TypeSnapshot, link.Snapshot{Seq: 20, Last: true})
	home.ack(t, 20)
	f.end(t, home)
	// A new process.
	if err := invalidateEvents(t.Context(), f.m.db); err != nil {
		t.Fatal(err)
	}
	if eligible() != 0 {
		t.Fatal("a new process found the cache eligible")
	}
}

// A lease runs from the arrival of the part that carried it.
func TestASpokeLeaseRunsFromItsFrame(t *testing.T) {
	f := newEventFixture(t)
	times := make(chan int64, 2)
	f.e.mono = func() (int64, error) { return <-times, nil }
	times <- int64(10 * time.Second)
	times <- int64(20 * time.Second)
	home := newFakeHome()
	f.start(t, home)
	home.push(t, link.TypeSnapshot, link.Snapshot{Seq: 10, Part: 0, Members: []link.SnapshotMember{
		{Member: eventMember, Scope: f.scope(7, 3), Offers: []link.Offer{offer(1, 0, 5_000)}}}})
	home.push(t, link.TypeSnapshot, link.Snapshot{Seq: 10, Part: 1, Last: true})
	home.ack(t, 10)
	if n := f.count(t, `SELECT deadline_mono / 1000000000 FROM spoke_offer`); n != 15 {
		t.Fatalf("deadline %d s", n)
	}
	f.end(t, home)
}

// Cuts that do not fit one frame are split by their serialized size.
func TestASpokeSplitsALargeAck(t *testing.T) {
	f := newEventFixture(t)
	var members []link.SnapshotMember
	for i := range 1000 {
		key := strings.Repeat("k", 1020) + fmt.Sprintf("%04d", i)
		members = append(members, link.SnapshotMember{Member: link.Member{Vendor: "claude", Key: key},
			Scope: link.MemberScope{LeafGeneration: "g", Membership: link.Membership{BindingID: 1, Generation: 1}}})
	}
	home := newFakeHome()
	f.start(t, home)
	home.push(t, link.TypeSnapshot, link.Snapshot{Seq: 10, Part: 0, Members: members[:500]})
	home.push(t, link.TypeSnapshot, link.Snapshot{Seq: 10, Part: 1, Last: true, Members: members[500:]})
	if cuts := home.ack(t, 10); len(cuts) != 1000 {
		t.Fatalf("%d cuts", len(cuts))
	}
	frames := 0
	for _, fr := range home.frames() {
		if fr.Type == link.TypeAck {
			frames++
		}
	}
	if frames < 2 {
		t.Fatalf("%d ack frames", frames)
	}
	f.end(t, home)
}

// A leaf moved away from its path while a change waits to commit is cut
// absent: the cut names the directory at the registered path.
func TestASpokeRechecksTheLeafPathUnderItsLock(t *testing.T) {
	f := newEventFixture(t)
	home := newFakeHome()
	f.start(t, home)
	home.push(t, link.TypeSnapshot, link.Snapshot{Seq: 10, Last: true, Members: []link.SnapshotMember{{Member: eventMember, Scope: f.scope(7, 3)}}})
	home.ack(t, 10)
	writing, done := make(chan struct{}), make(chan struct{})
	go f.m.db.Update(t.Context(), func(store.Tx) error {
		close(writing)
		<-done
		return nil
	})
	<-writing
	home.push(t, link.TypeEvent, event(t, 11, link.EventMembership, link.MembershipChange{Member: eventMember, Scope: f.scope(8, 4)}))
	time.Sleep(50 * time.Millisecond) // the client holds the leaf, waiting for the store
	if err := os.Rename(f.path, f.path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.path, 0700); err != nil {
		t.Fatal(err)
	}
	close(done)
	if cuts := home.ack(t, 11); len(cuts) != 1 || !cuts[0].Absent {
		t.Fatalf("cut %+v", cuts)
	}
	if n := f.count(t, `SELECT membership_gen FROM spoke_leaf`); n != 3 {
		t.Fatalf("a moved leaf took membership generation %d", n)
	}
	f.end(t, home)
}

// blockStamp makes the leaf's membership stamp unwritable.
func blockStamp(t *testing.T, f *spokeFixture) {
	t.Helper()
	_ = os.Remove(filepath.Join(f.path, "membership"))
	if err := os.Mkdir(filepath.Join(f.path, "membership"), 0700); err != nil {
		t.Fatal(err)
	}
}

// A snapshot whose stamps cannot be written is neither acknowledged nor
// counted.
func TestASpokeCountsASnapshotOnlyOnceItsStampsAreOut(t *testing.T) {
	f := newEventFixture(t)
	blockStamp(t, f.spokeFixture)
	home := newFakeHome()
	f.start(t, home)
	home.push(t, link.TypeSnapshot, link.Snapshot{Seq: 10, Last: true, Members: []link.SnapshotMember{{Member: eventMember, Scope: f.scope(7, 3)}}})
	select {
	case err := <-f.done:
		if err == nil {
			t.Fatal("the stream went on without the stamp")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the stream did not stop")
	}
	for _, fr := range home.frames() {
		if fr.Type == link.TypeAck {
			t.Fatal("an unstamped snapshot was acknowledged")
		}
	}
	if n := f.count(t, `SELECT eligible FROM spoke_event_cursor`); n != 0 {
		t.Fatal("an unstamped snapshot counted")
	}
}

// First contact fails when the session's stamp cannot be written, so the
// spool tries it again before a hook reads a missing stamp.
func TestASpokeFirstContactFailsWithoutItsStamp(t *testing.T) {
	f := newSpokeFixture(t)
	if err := f.m.db.Update(t.Context(), func(tx store.Tx) error {
		_, err := tx.ExecContext(t.Context(), `UPDATE spoke_leaf SET binding_id = 7, membership_gen = 3`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	blockStamp(t, f)
	id := wire.NewID()
	raw, err := json.Marshal(wire.Hello{Version: wire.Version, Attempt: id, Vendor: "codex", Key: "session", Intent: "join"})
	if err != nil {
		t.Fatal(err)
	}
	if r, err := f.s.spokeHello(t.Context(), id, raw); err == nil {
		t.Fatalf("first contact succeeded without its stamp: %+v", r)
	}
}

// A home restored to an older store ends a resumed stream at once; the
// next subscribe asks for a snapshot from zero, which any home serves.
func TestASpokeRecoversFromAnOlderHome(t *testing.T) {
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
		for len(home.frames()) == 0 {
			time.Sleep(time.Millisecond)
		}
		f.end(t, home) // the home refuses and ends the link
		var s link.Subscribe
		if err := json.Unmarshal(home.frames()[0].Body, &s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	if s := subscribe(); s.Snapshot || s.FromSeq != 10 {
		t.Fatalf("resume %+v", s)
	}
	// Offline after the refused resume: nothing cached counts.
	if n := f.count(t, `SELECT eligible FROM spoke_event_cursor`); n != 0 {
		t.Fatal("the cache counted after a refused resume")
	}
	if s := subscribe(); !s.Snapshot || s.FromSeq != 0 {
		t.Fatalf("after a refused resume %+v", s)
	}
}

// A snapshot below the acknowledged cursor (a home restored to an older
// store) sets the home's selections as they are, also when older,
// withdraws the stamp of a session it does not list, and keeps no offer
// of the newer state.
func TestASpokeRebaselinesOnAnOlderHome(t *testing.T) {
	f := newEventFixture(t)
	home := newFakeHome()
	f.start(t, home)
	newer := offer(1, 3, 60_000)
	newer.Nonce = "after-backup"
	home.push(t, link.TypeSnapshot, link.Snapshot{Seq: 10, Last: true, Members: []link.SnapshotMember{
		{Member: eventMember, Scope: f.scope(2, 2), Offers: []link.Offer{newer}}}})
	home.ack(t, 10)
	stamp := func() *core.Membership {
		t.Helper()
		m, err := ident.LookupMembership(f.dir)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	if m := stamp(); m == nil || *m != (core.Membership{BindingID: 2, Generation: 2}) {
		t.Fatalf("stamp %+v", m)
	}
	older := offer(1, 0, 60_000)
	older.Nonce = "before-backup"
	home.push(t, link.TypeSnapshot, link.Snapshot{Seq: 3, Last: true, Members: []link.SnapshotMember{
		{Member: eventMember, Scope: f.scope(1, 1), Offers: []link.Offer{older}}}})
	home.ack(t, 3)
	if m := stamp(); m == nil || *m != (core.Membership{BindingID: 1, Generation: 1}) {
		t.Fatalf("stamp %+v after the rollback", m)
	}
	if n := f.count(t, `SELECT binding_id * 10 + membership_gen FROM spoke_leaf`); n != 11 {
		t.Fatalf("selection %d after the rollback", n)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM spoke_offer WHERE json_extract(body, '$.nonce') = 'before-backup' AND lease_epoch = 0`); n != 1 {
		t.Fatal("the offer of the newer state stayed")
	}
	// An older home that no longer knows the session.
	home.push(t, link.TypeSnapshot, link.Snapshot{Seq: 1, Last: true})
	home.ack(t, 1)
	if m := stamp(); m != nil {
		t.Fatalf("an unknown session kept its stamp %+v", m)
	}
	if n := f.count(t, `SELECT binding_id + membership_gen + (SELECT COUNT(*) FROM spoke_offer) FROM spoke_leaf`); n != 0 {
		t.Fatalf("an unknown session kept %d", n)
	}
	f.end(t, home)
}
