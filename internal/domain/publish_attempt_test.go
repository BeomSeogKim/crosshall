package domain

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

const publishTestLaunch = "pid-42-start"
const publishTestSocket = "/tmp/cc-socks/42.sock"

type publishFixture struct {
	*fixture
	reader, writer session
	binding        PublishBinding
	queue          PublishQueue
	beforeID       int64
}

func newPublishFixture(t *testing.T) *publishFixture {
	t.Helper()
	usePublishSender(t)
	f, writer := adapterChannel(t)
	reader := f.newSession("claude")
	wantOK(t, f.join(reader, Join{Channel: "checkout", Alias: "reader"}, "reader-join"))

	beforeID := f.turnEndAsk(writer, "reader", "before-route")
	enrollAndRoute(t, f, reader, publishTestLaunch, "startup", publishTestSocket, "default", f.now.UnixMilli(), 1)
	binding := bindingIDOf(t, f, f.principalOf(reader))
	p := &publishFixture{fixture: f, reader: reader, writer: writer, binding: PublishBinding{ID: binding},
		queue: PublishQueue{PID: 42, StartMS: f.now.Add(-time.Second).UnixMilli()}, beforeID: beforeID}
	p.refreshBinding()
	p.mutate(`INSERT INTO capability_evidence (vendor, vendor_version, platform, mode, manifest_rev, cap, level, result_ref, checked_at_ms)
		SELECT DISTINCT 'claude', json_extract(profile, '$.vendor_version'), json_extract(profile, '$.platform'),
			json_extract(profile, '$.mode'), json_extract(profile, '$.manifest_rev'), 'publish', 'tested', 'publish-test', 1
		FROM adapter_launch WHERE launch_ref = ?`, publishTestLaunch)
	return p
}

func (p *publishFixture) refreshBinding() {
	p.t.Helper()
	p.binding.Generation, _, _, _, _, _, _ = routeState(p.t, p.fixture, p.binding.ID)
}

func (p *publishFixture) proveRoute() {
	p.t.Helper()
	p.refreshBinding()
	p.update(func(ctx context.Context, tx store.Tx) error { return ConfirmPublishRoute(ctx, tx, p.binding, p.queue) })
	p.activate()
}

// activate runs the publisher's activation step at p.now.
func (p *publishFixture) activate() {
	p.t.Helper()
	p.update(func(ctx context.Context, tx store.Tx) error { return ActivatePublishRoutes(ctx, tx, p.now) })
}

func (p *publishFixture) mutate(statement string, args ...any) {
	p.t.Helper()
	if _, err := p.db.Exec(statement, args...); err != nil {
		p.t.Fatal(err)
	}
}

func (p *publishFixture) update(apply func(context.Context, store.Tx) error) {
	p.t.Helper()
	ctx := p.t.Context()
	tx, err := p.db.BeginTx(ctx, nil)
	if err != nil {
		p.t.Fatal(err)
	}
	defer tx.Rollback()
	if err := apply(ctx, tx); err != nil {
		p.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		p.t.Fatal(err)
	}
}

func (p *publishFixture) reserve() int64 {
	p.t.Helper()
	var id int64
	p.update(func(ctx context.Context, tx store.Tx) error {
		ok, err := ReservePublish(ctx, tx, p.binding, p.now)
		if err != nil || !ok {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT id FROM publish_attempt WHERE binding_id = ? ORDER BY id DESC LIMIT 1`, p.binding.ID).Scan(&id)
	})
	return id
}

func (p *publishFixture) begin(id int64) []int64 {
	p.t.Helper()
	var ids []int64
	p.update(func(ctx context.Context, tx store.Tx) error {
		target, found, err := BeginPublish(ctx, tx, id, p.now)
		ids = found
		if err == nil && len(ids) != 0 && (target.Queue != p.queue || target.Address != publishTestSocket || target.Adapter != "claude") {
			p.t.Fatalf("incorrect target: %+v", target)
		}
		return err
	})
	return ids
}

func (p *publishFixture) resolve(id int64, outcome PublishOutcome) {
	p.t.Helper()
	p.update(func(ctx context.Context, tx store.Tx) error { return ResolvePublish(ctx, tx, id, outcome, p.now) })
}

func (p *publishFixture) state(id int64, want string) {
	p.t.Helper()
	var state string
	if err := p.db.QueryRow(`SELECT state FROM publish_attempt WHERE id = ?`, id).Scan(&state); err != nil || state != want {
		p.t.Fatalf("attempt %d: state %q (%v), want %q", id, state, err, want)
	}
}

func TestPublishEligibilityAndReservationBoundary(t *testing.T) {
	p := newPublishFixture(t)
	p.turnEndAsk(p.writer, "reader", "before-proof")
	if id := p.reserve(); id != 0 {
		t.Fatalf("candidate reserved without a proof: %d", id)
	}
	p.proveRoute()
	first := p.turnEndAsk(p.writer, "reader", "after-proof")
	p.mutate(`UPDATE binding SET publish_quiet = 1 WHERE id = ?`, p.binding.ID)
	if id := p.reserve(); id != 0 {
		t.Fatalf("quiet route reserved: %d", id)
	}
	p.mutate(`UPDATE binding SET publish_quiet = 0 WHERE id = ?`, p.binding.ID)
	p.noteTo(p.writer, "reader", 1)

	picked := p.turnEndAsk(p.writer, "reader", "before-reserve-read")
	cancelled := p.turnEndAsk(p.writer, "reader", "before-reserve-cancel")
	lateCancel := p.turnEndAsk(p.writer, "reader", "after-reserve-cancel")
	wantOK(t, p.run(p.reader, Read{IDs: []int64{picked}}, "read-before"))
	wantOK(t, p.owner(Cancel{Channel: "checkout", ID: cancelled}, "cancel-before"))
	id := p.reserve()
	if id == 0 {
		t.Fatal("no eligible work reserved")
	}
	wantOK(t, p.run(p.reader, Read{IDs: []int64{first}}, "read-after"))
	wantOK(t, p.owner(Cancel{Channel: "checkout", ID: lateCancel}, "cancel-after"))
	if ids := p.begin(id); !slices.Equal(ids, []int64{first, lateCancel}) {
		t.Fatalf("reserved ID list %v", ids)
	}
	if n := p.count(`SELECT COUNT(*) FROM announcement WHERE via = 'publish' AND ref = ?`, id); n != 2 {
		t.Fatalf("claims = %d, want 2", n)
	}
}

func TestPublishReplacementQueueGetsANewActivationWatermark(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	p.turnEndAsk(p.writer, "reader", "previous-queue")
	enrollAndRoute(t, p.fixture, p.reader, "pid-43-replacement", "resume", publishTestSocket, "default", p.now.UnixMilli()+1000, 1)
	p.refreshBinding()
	before := p.turnEndAsk(p.writer, "reader", "replacement-candidate")
	if id := p.reserve(); id != 0 {
		t.Fatalf("candidate replacement reserved before proof: %d", id)
	}
	p.queue = PublishQueue{PID: 43, StartMS: p.queue.StartMS + 1}
	p.proveRoute()
	if _, _, _, _, _, _, watermark := routeState(t, p.fixture, p.binding.ID); watermark != before {
		t.Fatalf("replacement watermark = %d, want %d", watermark, before)
	}
	after := p.turnEndAsk(p.writer, "reader", "replacement-proven")
	p.proveRoute()
	p.mutate(`UPDATE publish_route SET generation = generation + 1 WHERE binding_id = ?`, p.binding.ID)
	p.proveRoute()
	id := p.reserve()
	if id == 0 || !slices.Equal(p.begin(id), []int64{after}) {
		t.Fatal("replacement queue did not reserve exactly its post-activation work")
	}
}

func TestPublishIncludesOnlyFinalReplies(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	wantOK(t, p.run(p.reader, Read{IDs: []int64{p.beforeID}}, "read-before-route"))
	wantOK(t, p.reply(p.reader, Reply{To: p.beforeID, Body: "done", Outcome: OutcomeAnswer}, "settle-before-route"))
	reply := func(key, outcome string) int64 {
		ask := p.turnEndAsk(p.reader, "writer", key)
		wantOK(t, p.run(p.writer, Read{IDs: []int64{ask}}, key+"-read"))
		r := p.reply(p.writer, Reply{To: ask, Body: "reply", Outcome: outcome}, key+"-reply")
		wantOK(t, r)
		return decode[ReplyResult](t, r).ID
	}
	_ = reply("more-time", OutcomeMoreTime)
	answer := reply("answer", OutcomeAnswer)
	decline := reply("decline", OutcomeDecline)
	if id := p.reserve(); id == 0 || !slices.Equal(p.begin(id), []int64{answer, decline}) {
		t.Fatal("publish did not reserve exactly the answer and decline")
	}
}

func TestPublishAndStopShareTheAnnouncementClaim(t *testing.T) {
	for _, stopFirst := range []bool{true, false} {
		t.Run(fmt.Sprint("stop-first=", stopFirst), func(t *testing.T) {
			p := newPublishFixture(t)
			p.proveRoute()
			wantOK(t, p.owner(turnEndEvidence("tested"), "evidence"))
			wantOK(t, p.run(p.reader, Read{IDs: []int64{p.beforeID}}, "read-before-route"))
			ask := p.turnEndAsk(p.writer, "reader", "race")
			p.hook(p.reader, prompt(publishTestLaunch, "turn", p.now.UnixMilli()+1))
			cmd := stop(publishTestLaunch, "turn", p.now.UnixMilli()+2)
			if stopFirst {
				if got := p.hook(p.reader, cmd).TurnEndLine; got != fmt.Sprintf("[crosshall] pending: %d", ask) {
					t.Fatalf("Stop line %q", got)
				}
				if id := p.reserve(); id != 0 {
					t.Fatalf("publish competed with a committed Stop claim: %d", id)
				}
			} else {
				if id := p.reserve(); id == 0 {
					t.Fatal("publish did not claim")
				}
				if got := p.hook(p.reader, cmd).TurnEndLine; got != "" {
					t.Fatalf("Stop competed with a committed publish claim: %q", got)
				}
			}
			if n := p.count(`SELECT COUNT(*) FROM announcement WHERE message_id = ?`, ask); n != 1 {
				t.Fatalf("claims = %d", n)
			}
		})
	}
}

func TestPublishFinalFenceAndGenerationChanges(t *testing.T) {
	for _, invalidation := range []string{"clear", "bypass mode", "session end"} {
		t.Run(invalidation, func(t *testing.T) {
			for _, beforeFence := range []bool{true, false} {
				t.Run(fmt.Sprint("before-fence=", beforeFence), func(t *testing.T) {
					p := newPublishFixture(t)
					p.proveRoute()
					ask := p.turnEndAsk(p.writer, "reader", "fence")
					id := p.reserve()
					if id == 0 {
						t.Fatal("no reservation")
					}
					if !beforeFence && !slices.Equal(p.begin(id), []int64{ask}) {
						t.Fatal("valid final fence lost")
					}
					switch invalidation {
					case "clear":
						enrollAndRoute(t, p.fixture, p.reader, "pid-42-clear", "clear", publishTestSocket,
							"default", p.now.UnixMilli()+1000, 1)
					case "bypass mode":
						route := claudeRoute(publishTestLaunch, "mode", "", "bypassPermissions", p.now.UnixMilli()+1000, 1)
						wantOK(t, p.exec(p.caller(p.reader), Request{Command: route, SessionKey: p.reader.key, OpKey: "bypass-mode"}))
					case "session end":
						p.hook(p.reader, ended(publishTestLaunch, p.now.UnixMilli()+1000))
					}
					if beforeFence {
						if ids := p.begin(id); len(ids) != 0 {
							t.Fatalf("stale route crossed the final fence: %v", ids)
						}
						p.state(id, "not_submitted")
						if n := p.count(`SELECT COUNT(*) FROM announcement WHERE message_id = ?`, ask); n != 0 {
							t.Fatal("proven non-submission retained the claim")
						}
						return
					}
					p.resolve(id, PublishSubmitted)
					p.state(id, "submitted")
					if n := p.count(`SELECT COUNT(*) FROM announcement WHERE message_id = ?`, ask); n != 1 {
						t.Fatal("invalidation after fence discarded the claim")
					}
				})
			}
		})
	}
}

func TestPublishKeepsThePhysicalLaneUntilRetirement(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	old := p.turnEndAsk(p.writer, "reader", "hold-lane")
	id := p.reserve()
	if id == 0 || !slices.Equal(p.begin(id), []int64{old}) {
		t.Fatal("first reservation did not start")
	}
	p.resolve(id, PublishUncertain)
	p.hook(p.reader, prompt(publishTestLaunch, "prior-turn", p.now.UnixMilli()+100))
	p.hook(p.reader, stop(publishTestLaunch, "prior-turn", p.now.UnixMilli()+200))
	p.turnEndAsk(p.writer, "reader", "after-stop")
	if p.reserve() != 0 {
		t.Fatal("a Stop or uncertainty freed the lane")
	}
	off, on := false, true
	wantOK(t, p.owner(AdapterSet{wire.AdapterPolicyArgs{Adapter: "claude", Enabled: &off}}, "off"))
	wantOK(t, p.owner(AdapterSet{wire.AdapterPolicyArgs{Adapter: "claude", Enabled: &on}}, "on"))
	epoch := uint64(p.count(`SELECT epoch FROM adapter_policy WHERE adapter = 'claude'`))
	enrollAndRoute(t, p.fixture, p.reader, "pid-42-resumed", "resume", publishTestSocket, "default",
		p.now.UnixMilli()+1000, epoch)
	p.proveRoute()
	p.hook(p.reader, ended(publishTestLaunch, p.now.UnixMilli()+2000))
	p.turnEndAsk(p.writer, "reader", "new-policy")
	if p.reserve() != 0 {
		t.Fatal("the same queue's new generation freed its outstanding lane")
	}
	p.update(func(ctx context.Context, tx store.Tx) error { return RetirePublishQueue(ctx, tx, p.queue, p.now) })
	if p.reserve() != 0 {
		t.Fatal("retired physical queue remained eligible")
	}
	if n := p.count(`SELECT COUNT(*) FROM announcement WHERE message_id = ?`, old); n != 1 {
		t.Fatal("retirement released possibly submitted claims")
	}
	p.queue = PublishQueue{PID: 42, StartMS: p.queue.StartMS + 1}
	p.proveRoute()
	p.turnEndAsk(p.writer, "reader", "new-process")
	if p.reserve() == 0 {
		t.Fatal("a new birth was held by the retired process")
	}
}

func TestPublishPhysicalLaneSpansBindings(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	other := p.newSession("claude")
	wantOK(t, p.join(other, Join{Channel: "checkout", Alias: "other"}, "other-join"))
	enrollAndRoute(t, p.fixture, other, publishTestLaunch, "startup", publishTestSocket, "default", p.now.UnixMilli(), 1)
	binding := PublishBinding{ID: bindingIDOf(t, p.fixture, p.principalOf(other))}
	binding.Generation, _, _, _, _, _, _ = routeState(t, p.fixture, binding.ID)
	p.update(func(ctx context.Context, tx store.Tx) error { return ConfirmPublishRoute(ctx, tx, binding, p.queue) })
	p.turnEndAsk(p.writer, "reader", "first-physical-queue")
	p.turnEndAsk(p.writer, "other", "same-physical-queue")
	if p.reserve() == 0 {
		t.Fatal("first binding did not reserve")
	}
	p.update(func(ctx context.Context, tx store.Tx) error {
		if reserved, err := ReservePublish(ctx, tx, binding, p.now); err != nil || !reserved {
			return err
		}
		return fmt.Errorf("second binding reserved the same physical queue")
	})
}

func TestPublishCapsAndProvenNonSubmission(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	var items []int64
	for i := range OfferCap + 1 {
		items = append(items, p.turnEndAsk(p.writer, "reader", fmt.Sprintf("overflow-%d", i)))
	}
	id := p.reserve()
	if id == 0 || !slices.Equal(p.begin(id), items[:OfferCap]) {
		t.Fatal("the sorted first batch did not honor OfferCap")
	}
	p.resolve(id, PublishNotSubmitted)
	next := p.reserve()
	if next == 0 || !slices.Equal(p.begin(next), items[OfferCap:]) {
		t.Fatal("same generation retried known non-submission or lost overflow")
	}
	p.resolve(next, PublishNotSubmitted)
	if id := p.reserve(); id != 0 {
		t.Fatalf("retried the same generation: %d", id)
	}
	p.mutate(`UPDATE publish_route SET generation = generation + 1 WHERE binding_id = ?`, p.binding.ID)
	p.proveRoute()
	p.mutate(`WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < ?)
		INSERT INTO publish_attempt (binding_id, principal_id, binding_epoch, generation, adapter, session_ref, launch_ref,
			policy_epoch, process_pid, process_start_ms, state, reserved_at_ms, submitting_at_ms, resolved_at_ms, observed_at_ms, observation)
		SELECT binding_id, principal_id, binding_epoch, generation, adapter, session_ref, launch_ref,
			policy_epoch, 42, ?, CASE WHEN n % 2 = 0 THEN 'submitted' ELSE 'uncertain' END, ?, ?, ?, ?, 'own_turn'
		FROM publish_route CROSS JOIN seq WHERE binding_id = ?`,
		store.PublishHourlyCap, p.queue.StartMS, p.now.UnixMilli(), p.now.UnixMilli(), p.now.UnixMilli(), p.now.UnixMilli(), p.binding.ID)
	if id := p.reserve(); id != 0 {
		t.Fatalf("possibly submitted attempts did not exhaust cap: %d", id)
	}
	p.now = p.now.Add(time.Hour)
	id = p.reserve()
	if id == 0 || !slices.Equal(p.begin(id), items[:OfferCap]) {
		t.Fatal("cap did not reset or a newer generation could not recover known non-submission")
	}
}

// usePublishSender gives Claude a transport in the effective registry, so
// its routes can publish; the domain never calls it.
func usePublishSender(t *testing.T) {
	t.Helper()
	t.Cleanup(publish.Swap(publish.Shipped().WithTransport("claude", publish.Transport{
		Send: func(context.Context, publish.Target, string) publish.Outcome { return publish.NotSubmitted },
	})))
}

// The final fence resolves the registered route through its adapter, the
// same rule selection and status use, mode included: a reserved notice
// whose route no longer resolves is not submitted.
func TestTheFinalFenceResolvesTheRouteThroughItsAdapter(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	p.turnEndAsk(p.writer, "reader", "fence")
	id := p.reserve()
	if id == 0 {
		t.Fatal("nothing reserved")
	}
	p.mutate(`UPDATE publish_route SET mode = 'bypassPermissions' WHERE binding_id = ?`, p.binding.ID)
	if ids := p.begin(id); len(ids) != 0 {
		t.Fatalf("submitted %v to a route its adapter does not resolve", ids)
	}
	p.state(id, string(PublishNotSubmitted))
}

func (p *publishFixture) unproven() PublishProof {
	p.t.Helper()
	var proofs []PublishProof
	p.update(func(ctx context.Context, tx store.Tx) (err error) {
		proofs, err = UnprovenPublishRoutes(ctx, tx, p.now)
		return err
	})
	if len(proofs) != 1 {
		p.t.Fatalf("unproven routes %+v, want this one", proofs)
	}
	return proofs[0]
}

// proven is the fixture listener's proof: its queue, started at the
// queue's start time.
func (p *publishFixture) proven() publish.Proven {
	return publish.Proven{Queue: p.queue, StartedAtMS: p.queue.StartMS}
}

func (p *publishFixture) confirm(proof PublishProof, proven publish.Proven) bool {
	p.t.Helper()
	var recorded bool
	p.update(func(ctx context.Context, tx store.Tx) (err error) {
		recorded, err = ConfirmPublishProof(ctx, tx, proof, proven)
		return err
	})
	return recorded
}

// A proof counts only for the launch it was asked for: the listener must be
// the launch's process, started no later than the launch's first hook.
func TestAProofMustBeTheExpectedLaunch(t *testing.T) {
	p := newPublishFixture(t)
	proof := p.unproven()
	if proof.Probe.LaunchRef != publishTestLaunch || proof.Probe.Address != publishTestSocket ||
		proof.Probe.Route.SocketPath != publishTestSocket || proof.Probe.StartedHookAtMS != p.now.UnixMilli() {
		t.Fatalf("probe %+v lacks the registered launch evidence", proof.Probe)
	}
	start := p.queue.StartMS
	for name, proven := range map[string]publish.Proven{
		"another process":        {Queue: PublishQueue{PID: 43, StartMS: start}, StartedAtMS: start},
		"started after the hook": {Queue: p.queue, StartedAtMS: p.now.UnixMilli() + 1},
		"no start estimate":      {Queue: p.queue},
		"no start identity":      {Queue: PublishQueue{PID: 42}, StartedAtMS: start},
	} {
		if p.confirm(proof, proven) {
			t.Fatalf("%s: recorded %+v for launch %s", name, proven, proof.Probe.LaunchRef)
		}
	}
	if !p.confirm(proof, p.proven()) {
		t.Fatal("the launch's own process was not recorded")
	}
}

// Astra #433 and #438: the watermark is set at the route's first
// availability, so work stored while it was off, capped or held (by this
// binding or by another on the same physical queue) is never offered.
func TestARouteActivatesAtItsFirstAvailability(t *testing.T) {
	const attempt = `INSERT INTO publish_attempt (binding_id, principal_id, binding_epoch, generation, adapter, session_ref,
		launch_ref, policy_epoch, process_pid, process_start_ms, state, reserved_at_ms, submitting_at_ms, observed_at_ms, lane)
		VALUES (?, ?, 1, 1, 'claude', 'fixture', 'fixture', 0, ?, ?, 'submitted', ?, ?, ?, 'process:' || ?3 || ':' || ?4)`
	for _, c := range []struct {
		name         string
		block, clear func(p *publishFixture)
		want         string
	}{
		{"off", func(p *publishFixture) {
			p.mutate(`UPDATE binding SET publish_quiet = 1 WHERE id = ?`, p.binding.ID)
		}, func(p *publishFixture) {
			p.mutate(`UPDATE binding SET publish_quiet = 0 WHERE id = ?`, p.binding.ID)
		}, "off"},
		{"capped", func(p *publishFixture) {
			at := p.now.UnixMilli()
			for i := int64(0); i < store.PublishHourlyCap; i++ {
				p.mutate(attempt, p.binding.ID, p.principalOf(p.reader), 1000+i, 1, at, at, at)
			}
		}, func(p *publishFixture) { p.now = p.now.Add(time.Hour + time.Second) }, "capped"},
		{"held by another binding on the queue", func(p *publishFixture) {
			at := p.now.UnixMilli()
			p.mutate(attempt, bindingIDOf(t, p.fixture, p.principalOf(p.writer)), p.principalOf(p.writer),
				p.queue.PID, p.queue.StartMS, at, at, nil)
		}, func(p *publishFixture) {
			p.mutate(`UPDATE publish_attempt SET observed_at_ms = ? WHERE observed_at_ms IS NULL`, p.now.UnixMilli())
		}, "held"},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := newPublishFixture(t)
			probe := p.unproven() // the proof is asked for, then the route stops
			c.block(p)
			if !p.confirm(probe, p.proven()) {
				t.Fatal("the listener's identity was not recorded")
			}
			p.activate()
			if state := p.publishState().State; state != c.want {
				t.Fatalf("state %q, want %q", state, c.want)
			}
			p.turnEndAsk(p.writer, "reader", "while-"+c.name)
			p.activate()
			c.clear(p)
			if got := p.publishState(); got.State != "candidate" || got.Reason != store.PublishNotActivated {
				t.Fatalf("before activation: %+v", got)
			}
			p.activate()
			if state := p.publishState().State; state != "available" {
				t.Fatalf("after activation: %q", state)
			}
			if id := p.reserve(); id != 0 {
				t.Fatalf("offered work stored while the route was %s", c.name)
			}
			after := p.turnEndAsk(p.writer, "reader", "after-activation")
			if ids := p.begin(p.reserve()); len(ids) != 1 || ids[0] != after {
				t.Fatalf("offered %v, want only %d", ids, after)
			}
		})
	}
}

// modeAddress resolves a different address in every permission mode.
type modeAddress struct{ publish.Adapter }

func (m modeAddress) Resolve(r publish.Route) (string, string) {
	address, why := m.Adapter.Resolve(r)
	if why != "" {
		return "", why
	}
	return address + "#" + r.Mode, ""
}

// Astra #433: a proof is for one address, so a mode change that resolves a
// new one needs a new proof before anything is sent there.
func TestAModeThatMovesTheAddressNeedsANewProof(t *testing.T) {
	p := newPublishFixture(t)
	claude, _ := publish.Lookup("claude")
	t.Cleanup(publish.Swap(publish.Adapters().With("claude", modeAddress{claude})))
	p.proveRoute()
	before := p.binding.Generation
	r := p.exec(p.caller(p.reader), Request{
		Command:    claudeRoute(publishTestLaunch, "mode-move", "", "acceptEdits", p.now.UnixMilli()+1_000, 1),
		Vendor:     p.reader.vendor,
		SessionKey: p.reader.key,
		OpKey:      "hook:claude:" + publishTestLaunch + ":mode-move",
	})
	wantOK(t, r)
	p.refreshBinding()
	if p.binding.Generation != before+1 {
		t.Fatalf("generation %d after the address moved, want %d", p.binding.Generation, before+1)
	}
	p.turnEndAsk(p.writer, "reader", "after-move")
	if id := p.reserve(); id != 0 {
		t.Fatal("reserved on the old proof for a new address")
	}
	if got := p.unproven().Probe.Address; got != publishTestSocket+"#acceptEdits" {
		t.Fatalf("new proof asked for %q", got)
	}
}

// An inbox tells the member whether the hall delivers to it: only with a
// proven route, and not while an earlier notice still holds its lane.
func TestInboxReportsWhetherTheHallDelivers(t *testing.T) {
	p := newPublishFixture(t)
	delivers := func() bool { return decode[InboxResult](t, p.run(p.reader, Inbox{}, "")).Delivers }
	if delivers() {
		t.Fatal("a candidate route reads as delivering")
	}
	p.proveRoute()
	if !delivers() {
		t.Fatal("a proven route does not read as delivering")
	}
	p.turnEndAsk(p.writer, "reader", "holds the lane")
	id := p.reserve()
	if id == 0 || delivers() {
		t.Fatalf("attempt %d: a member with a notice outstanding reads as delivering", id)
	}
	if len(p.begin(id)) == 0 {
		t.Fatal("the reservation did not start")
	}
	p.resolve(id, PublishNotSubmitted)
	if !delivers() {
		t.Fatal("a released lane does not read as delivering")
	}
	if decode[InboxResult](t, p.run(p.writer, Inbox{}, "")).Delivers {
		t.Fatal("a member without a route reads as delivering")
	}
	// Another binding's notice on the same physical queue holds it too.
	other := bindingIDOf(t, p.fixture, p.principalOf(p.writer))
	const attempt = `INSERT INTO publish_attempt (binding_id, principal_id, binding_epoch, generation, adapter, session_ref,
		launch_ref, policy_epoch, process_pid, process_start_ms, state, reserved_at_ms, submitting_at_ms, observed_at_ms, lane)
		VALUES (?, ?, 1, 1, 'claude', 'fixture', 'fixture', 0, ?, ?, 'submitted', ?, ?, ?, 'process:' || ?3 || ':' || ?4)`
	at := p.now.UnixMilli()
	p.mutate(attempt, other, p.principalOf(p.writer), p.queue.PID, p.queue.StartMS, at, at, nil)
	if delivers() {
		t.Fatal("a member whose shared queue is held reads as delivering")
	}
	p.mutate(`UPDATE publish_attempt SET observed_at_ms = ? WHERE binding_id = ?`, at, other)
	if !delivers() {
		t.Fatal("a released shared queue does not read as delivering")
	}
	// A member that spent its hourly notices is on pull until the hour turns.
	reader := p.principalOf(p.reader)
	for i := int64(0); i < store.PublishHourlyCap-1; i++ {
		p.mutate(attempt, p.binding.ID, reader, 1000+i, 1, at, at, at)
	}
	if !delivers() {
		t.Fatal("a member one notice under the cap does not read as delivering")
	}
	p.mutate(attempt, p.binding.ID, reader, 2000, 1, at, at, at)
	if delivers() {
		t.Fatal("a member at the hourly cap reads as delivering")
	}
	p.now = p.now.Add(time.Hour + time.Second)
	if !delivers() {
		t.Fatal("a new hour does not read as delivering")
	}
}
