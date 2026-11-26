package dispatch

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/link"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// hookFixture is an event fixture whose spoke has the codex policy at
// epoch 3 and one offer for launch L1, applied by a snapshot at seq 10.
type hookFixture struct {
	*eventFixture
	home     *fakeHome
	mu       sync.Mutex
	policies []bool // publish (true) or withdraw (false), in order
}

const hookLaunch = "L1"

func newHookFixture(t *testing.T) *hookFixture {
	t.Helper()
	f := &hookFixture{eventFixture: newEventFixture(t), home: newFakeHome()}
	f.m.mono = f.e.mono
	f.m.hooks = make(chan struct{}, 1)
	f.e.policy = func(_ context.Context, publish bool) error {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.policies = append(f.policies, publish)
		return nil
	}
	f.start(t, f.home)
	o := offer(7, 0, 60_000)
	o.IDs, o.Nonce, o.Launch, o.PolicyEpoch = []int64{41, 42}, "nonce-7", hookLaunch, 3
	o.LeafGeneration, o.Membership = f.generation, link.Membership{BindingID: 2, Generation: 2}
	f.home.push(t, link.TypeSnapshot, link.Snapshot{Seq: 10, Last: true,
		Members:  []link.SnapshotMember{{Member: eventMember, Scope: f.scope(2, 2), Offers: []link.Offer{o}}},
		Policies: []link.AdapterPolicy{{Adapter: "codex", Enabled: true, Epoch: 3}}})
	f.home.ack(t, 10)
	return f
}

func (f *hookFixture) published() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.policies...)
}

// hook sends one hook request of the fixture's session, captured at
// publish sequence seq with the given stamp.
func (f *hookFixture) hook(t *testing.T, verb string, args any, seq uint64, stamp *core.Membership) core.Result {
	t.Helper()
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	var ref struct {
		LaunchRef string `json:"launch_ref"`
		EventRef  string `json:"event_ref"`
	}
	_ = json.Unmarshal(raw, &ref)
	id := wire.NewID()
	req := wire.Request{Version: wire.Version, Attempt: id, OpKey: "hook:codex:" + ref.LaunchRef + ":" + ref.EventRef,
		KeyDigest: f.digest, Generation: f.generation, Verb: verb, Args: raw, PublishSeq: seq, OfferReceipts: true,
		ExpiresAtMS: time.Now().Add(2 * time.Second).UnixMilli(),
		Capture:     &wire.HookCapture{Boot: "boot-1", MonoNS: 5, WallMS: 6, Membership: stamp}}
	if verb == wire.VerbHookJoinHintAck {
		no := false
		req.Reply = &no
	}
	body, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.s.spokeCommand(t.Context(), f.path, f.dir, id, body)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func prompt(ref string, epoch uint64) wire.HookEventArgs {
	return wire.HookEventArgs{Profile: wire.HookProfile{Adapter: "codex"}, LaunchRef: hookLaunch, EventRef: ref,
		Fact: wire.HookPromptSubmitted, TurnRef: "t-" + ref, WantOutput: true, Epoch: epoch,
		PIDNamespace: "pidns", VendorHome: "/home/codex", EnrollRepoRoot: "/repo"}
}

var stamp22 = &core.Membership{BindingID: 2, Generation: 2}

// A hook of a linked device's session shows the offer the home prepared,
// from the cache, and queues its fact with the token it captured; device
// facts are cleared, and a policy epoch the device does not hold refuses.
func TestASpokeServesAHookFromItsCache(t *testing.T) {
	f := newHookFixture(t)
	if got := f.published(); len(got) < 2 || got[0] || !got[len(got)-1] {
		t.Fatalf("policy files %v: withdrawn before the snapshot, published once it counts", got)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM spoke_policy WHERE adapter = 'codex' AND enabled = 1 AND epoch = 3`); n != 1 {
		t.Fatal("the snapshot's policy is not cached")
	}
	r := f.hook(t, wire.VerbHookEvent, prompt("e1", 3), 5, stamp22)
	var out wire.HookResult
	if r.Status != core.ExitOK || json.Unmarshal(r.Data, &out) != nil {
		t.Fatalf("hook %+v", r)
	}
	if !strings.Contains(out.PendingLine, "41") || out.OfferReceipt == nil || out.OfferReceipt.Nonce != "nonce-7" ||
		out.Epoch != 3 || out.Ack == nil {
		t.Fatalf("hook output %+v", out)
	}
	var kind, body string
	if err := f.m.db.View(t.Context(), func(tx store.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT kind, body FROM spoke_hook_outbox`).Scan(&kind, &body)
	}); err != nil {
		t.Fatal(err)
	}
	var fact link.Fact
	var args wire.HookEventArgs
	if kind != "fact" || json.Unmarshal([]byte(body), &fact) != nil || json.Unmarshal(fact.Body, &args) != nil {
		t.Fatalf("queued %s %s", kind, body)
	}
	want := link.HookToken{Boot: "boot-1", LeafSeq: 5, MonoNS: 5, WallMS: 6, LeafGeneration: f.generation,
		Membership: link.Membership{BindingID: 2, Generation: 2}}
	if fact.Member != eventMember || fact.Kind != wire.VerbHookEvent || fact.Token != want {
		t.Fatalf("fact %+v", fact)
	}
	if args.PIDNamespace != "" || args.VendorHome != "" || args.EnrollRepoRoot != "" || args.Fact != wire.HookPromptSubmitted {
		t.Fatalf("device facts left in %+v", args)
	}
	// Another launch, or another stamp, sees no offer; another epoch is refused.
	other := prompt("e2", 3)
	other.LaunchRef = "L2"
	if r := f.hook(t, wire.VerbHookEvent, other, 6, stamp22); r.Status != core.ExitOK || strings.Contains(string(r.Data), "pending") {
		t.Fatalf("another launch: %+v", r)
	}
	if r := f.hook(t, wire.VerbHookEvent, prompt("e3", 3), 6, &core.Membership{BindingID: 2, Generation: 3}); r.Status != core.ExitOK ||
		strings.Contains(string(r.Data), "pending") {
		t.Fatalf("another stamp: %+v", r)
	}
	if r := f.hook(t, wire.VerbHookEvent, prompt("e4", 2), 7, stamp22); r.Status == core.ExitOK ||
		!strings.Contains(string(r.Data), "adapter policy") {
		t.Fatalf("an old policy epoch: %+v", r)
	}
	if r := f.hook(t, wire.VerbHookEvent, prompt("e5", 3), 8, nil); r.Code != core.CodeNotEnrolled {
		t.Fatalf("a hook without a stamp: %+v", r)
	}
	f.end(t, f.home)
}

// The written acknowledgement suppresses the offer on the device at once
// and queues a Written with its own token.
func TestASpokeHookAcknowledgementSuppressesTheOffer(t *testing.T) {
	f := newHookFixture(t)
	f.hook(t, wire.VerbHookJoinHintAck, wire.HookOutputAckArgs{Offer: &wire.HookOfferReceipt{OfferID: 7, Nonce: "nonce-7"},
		WrittenAtMS: 99}, 9, stamp22)
	if n := f.count(t, `SELECT suppressed FROM spoke_offer WHERE offer_id = 7`); n != 1 {
		t.Fatal("the written offer is still served")
	}
	var w link.Written
	var body string
	if err := f.m.db.View(t.Context(), func(tx store.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT body FROM spoke_hook_outbox WHERE kind = 'written'`).Scan(&body)
	}); err != nil || json.Unmarshal([]byte(body), &w) != nil {
		t.Fatalf("written %v %s", err, body)
	}
	if w.OfferID != 7 || w.Nonce != "nonce-7" || w.WrittenAtMS != 99 || w.Token.LeafSeq != 9 || w.Member != eventMember {
		t.Fatalf("written %+v", w)
	}
	var out wire.HookResult
	r := f.hook(t, wire.VerbHookEvent, prompt("e6", 3), 10, stamp22)
	if json.Unmarshal(r.Data, &out) != nil || out.PendingLine != "" {
		t.Fatalf("a suppressed offer was served: %+v", out)
	}
	f.end(t, f.home)
}

// The event client sends queued hook frames once the home caught up, each
// leaf's in publish sequence order, retires one on its Recorded, and sends
// the rest again on the next connection.
func TestASpokeSendsHookFramesInLeafOrder(t *testing.T) {
	f := newHookFixture(t)
	for _, seq := range []uint64{12, 11} { // the drain ordered them by time
		f.hook(t, wire.VerbHookEvent, prompt("e"+string(rune('a'+seq)), 3), seq, stamp22)
	}
	f.home.push(t, link.TypeCaughtUp, link.CaughtUp{Seq: 10})
	facts := func(h *fakeHome) []link.Fact {
		var out []link.Fact
		for _, fr := range h.frames() {
			var fact link.Fact
			if fr.Type == link.TypeFact && json.Unmarshal(fr.Body, &fact) == nil {
				out = append(out, fact)
			}
		}
		return out
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(facts(f.home)) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	got := facts(f.home)
	if len(got) != 2 || got[0].Token.LeafSeq != 11 || got[1].Token.LeafSeq != 12 {
		t.Fatalf("sent %+v", got)
	}
	f.home.push(t, link.TypeRecorded, link.Recorded{ID: got[1].ID, Status: link.RecordedApplied})
	for f.count(t, `SELECT COUNT(*) FROM spoke_hook_outbox`) != 1 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM spoke_hook_outbox`); n != 1 {
		t.Fatalf("%d frames queued after a Recorded", n)
	}
	// A frame queued later goes out on the wake of its drain pass.
	f.hook(t, wire.VerbHookEvent, prompt("late", 3), 13, stamp22)
	f.s.spokeHookQueued()
	for len(facts(f.home)) < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := facts(f.home); len(got) != 3 || got[2].Token.LeafSeq != 13 {
		t.Fatalf("a late frame: %+v", got)
	}
	f.end(t, f.home)
	again := newFakeHome()
	f.start(t, again)
	again.push(t, link.TypeCaughtUp, link.CaughtUp{Seq: 10})
	for len(facts(again)) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := facts(again); len(got) != 2 || got[0].ID != facts(f.home)[0].ID {
		t.Fatalf("resent %+v", got)
	}
	f.end(t, again)
}

// A policy event replaces the cached policy and publishes the files; a
// gap withdraws them before the cache stops counting.
func TestASpokeKeepsTheHallsAdapterPolicy(t *testing.T) {
	f := newHookFixture(t)
	f.home.push(t, link.TypeEvent, event(t, 11, link.EventPolicy, link.AdapterPolicy{Adapter: "codex", Enabled: true, Reminder: true, Epoch: 4}))
	f.home.ack(t, 11)
	if n := f.count(t, `SELECT epoch * 10 + reminder FROM spoke_policy WHERE adapter = 'codex'`); n != 41 {
		t.Fatalf("policy %d", n)
	}
	before := len(f.published())
	if got := f.published(); !got[before-1] {
		t.Fatal("the new policy was not published")
	}
	f.home.push(t, link.TypeEvent, event(t, 13, link.EventPolicy, link.AdapterPolicy{Adapter: "codex", Epoch: 5}))
	if err := <-f.done; err != errEventGap {
		t.Fatalf("gap: %v", err)
	}
	if got := f.published(); len(got) != before+1 || got[before] {
		t.Fatalf("policy files after a gap %v", got)
	}
	if n := f.count(t, `SELECT eligible FROM spoke_event_cursor`); n != 0 {
		t.Fatal("the cache still counts")
	}
}

// A membership change the device stamped from a command answer before the
// home's event arrived is cut where the stamp changed: a hook published
// after the stamp (it carries the new membership) is after the cut, and one
// published before it (it carries the old one) is not. A route change
// alone is cut where it applies.
func TestASpokeCutsAMembershipChangeWhereItsStampChanged(t *testing.T) {
	f := newEventFixture(t)
	home := newFakeHome()
	f.start(t, home)
	home.push(t, link.TypeSnapshot, link.Snapshot{Seq: 10, Last: true, Members: []link.SnapshotMember{{Member: eventMember, Scope: f.scope(1, 1)}}})
	home.ack(t, 10)
	publish := func(seq int) {
		if err := os.WriteFile(filepath.Join(f.path, "hook-publish-seq"), []byte(strconv.Itoa(seq)+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	publish(3) // hooks 1-3 carry 1/1
	f.exec(t, `UPDATE spoke_leaf SET binding_id = 2, membership_gen = 2`)
	if err := stampLeaf(t.Context(), f.m.db, f.m.members, eventMember.Vendor, eventMember.Key); err != nil {
		t.Fatal(err) // the join's answer
	}
	publish(5) // hooks 4-5 carry 2/2
	// A move arrives as an invalidate, then the membership event of the
	// same scope: both cut where the stamp changed.
	home.push(t, link.TypeEvent, event(t, 11, link.EventInvalidate, link.Invalidate{Member: eventMember, Reason: link.InvalidateMove, Scope: f.scope(2, 2)}))
	if cuts := home.ack(t, 11); len(cuts) != 1 || cuts[0].LeafSeq != 3 {
		t.Fatalf("membership change cut %+v, want leaf seq 3", cuts)
	}
	home.push(t, link.TypeEvent, event(t, 12, link.EventMembership, link.MembershipChange{Member: eventMember, Scope: f.scope(2, 2)}))
	if cuts := home.ack(t, 12); len(cuts) != 1 || cuts[0].LeafSeq != 3 {
		t.Fatalf("the same scope again cut %+v, want leaf seq 3", cuts)
	}
	scope := f.scope(2, 2)
	scope.RouteEpoch = 2
	home.push(t, link.TypeEvent, event(t, 13, link.EventMembership, link.MembershipChange{Member: eventMember, Scope: scope}))
	if cuts := home.ack(t, 13); len(cuts) != 1 || cuts[0].LeafSeq != 5 {
		t.Fatalf("route change cut %+v, want leaf seq 5", cuts)
	}
	// The same route scope again never cuts earlier than that.
	home.push(t, link.TypeEvent, event(t, 14, link.EventMembership, link.MembershipChange{Member: eventMember, Scope: scope}))
	if cuts := home.ack(t, 14); len(cuts) != 1 || cuts[0].LeafSeq != 5 {
		t.Fatalf("the same route scope again cut %+v, want leaf seq 5", cuts)
	}
	f.end(t, home)
}

// What else keeps a hook quiet or refused: an enrollment loses its
// repository; a disabled policy refuses; a stamp without a binding or
// generation is not a live member; an offer of another leaf generation or
// policy epoch is not shown.
func TestASpokeHookChecksEveryField(t *testing.T) {
	f := newHookFixture(t)
	enroll := wire.HookEnrollArgs{Profile: wire.HookProfile{Adapter: "codex"}, LaunchRef: hookLaunch, EventRef: "s1",
		RepoRoot: "/repo", Repository: "/repo", CreateChannel: true, Epoch: 3}
	if r := f.hook(t, wire.VerbHookEnroll, enroll, 4, stamp22); r.Status != core.ExitOK {
		t.Fatalf("enroll %+v", r)
	}
	var body string
	if err := f.m.db.View(t.Context(), func(tx store.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT body FROM spoke_hook_outbox WHERE kind = 'fact'`).Scan(&body)
	}); err != nil {
		t.Fatal(err)
	}
	var fact link.Fact
	var args wire.HookEnrollArgs
	if json.Unmarshal([]byte(body), &fact) != nil || json.Unmarshal(fact.Body, &args) != nil ||
		args.RepoRoot != "" || args.Repository != "" || args.CreateChannel || args.LaunchRef != hookLaunch {
		t.Fatalf("enrollment repository left in %s", body)
	}
	for _, s := range []*core.Membership{{BindingID: 0, Generation: 2}, {BindingID: 2, Generation: 0}} {
		if r := f.hook(t, wire.VerbHookEvent, prompt("z", 3), 5, s); r.Code != core.CodeNotEnrolled {
			t.Fatalf("stamp %+v: %+v", s, r)
		}
	}
	quiet := func(what string, seq uint64) {
		t.Helper()
		if r := f.hook(t, wire.VerbHookEvent, prompt(what, 3), seq, stamp22); r.Status != core.ExitOK ||
			strings.Contains(string(r.Data), "pending") {
			t.Fatalf("%s: %+v", what, r)
		}
	}
	f.exec(t, `UPDATE spoke_offer SET body = json_set(body, '$.leaf_generation', 'other')`)
	quiet("another leaf generation", 6)
	f.exec(t, `UPDATE spoke_offer SET body = json_set(body, '$.leaf_generation', ?, '$.policy_epoch', 2)`, f.generation)
	quiet("another policy epoch", 7)
	f.exec(t, `UPDATE spoke_policy SET enabled = 0`)
	if r := f.hook(t, wire.VerbHookEvent, prompt("off", 3), 8, stamp22); r.Status == core.ExitOK ||
		!strings.Contains(string(r.Data), "adapter policy") {
		t.Fatalf("a disabled policy: %+v", r)
	}
	f.end(t, f.home)
}

// A snapshot replaces the device's policies: one it no longer carries is
// gone.
func TestASpokeSnapshotReplacesThePolicies(t *testing.T) {
	f := newEventFixture(t)
	f.exec(t, `INSERT INTO spoke_policy (adapter, enabled, reminder, epoch) VALUES ('claude', 1, 0, 4)`)
	home := newFakeHome()
	f.start(t, home)
	home.push(t, link.TypeSnapshot, link.Snapshot{Seq: 10, Last: true,
		Policies: []link.AdapterPolicy{{Adapter: "codex", Enabled: true, Epoch: 3}}})
	home.ack(t, 10)
	if n := f.count(t, `SELECT COUNT(*) FROM spoke_policy`); n != 1 {
		t.Fatalf("%d policies after the snapshot, want codex alone", n)
	}
	f.end(t, home)
}
