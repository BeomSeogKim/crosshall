package domain

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

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
	f, writer := adapterChannel(t)
	reader := f.newSession("claude")
	wantOK(t, f.join(reader, Join{Channel: "checkout", Alias: "reader"}, "reader-join"))
	f.roots(writer, 40)
	f.roots(reader, 4)
	beforeID := f.turnEndAsk(writer, "reader", "before-route")
	enrollAndRoute(t, f, reader, publishTestLaunch, "startup", publishTestSocket, "default", f.now.UnixMilli(), 1)
	binding := bindingIDOf(t, f, f.principalOf(reader))
	p := &publishFixture{fixture: f, reader: reader, writer: writer, binding: PublishBinding{ID: binding},
		queue: PublishQueue{PID: 42, StartMS: f.now.Add(-time.Second).UnixMilli()}, beforeID: beforeID}
	p.refreshBinding()
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
		publishHourlyCap, p.queue.StartMS, p.now.UnixMilli(), p.now.UnixMilli(), p.now.UnixMilli(), p.now.UnixMilli(), p.binding.ID)
	if id := p.reserve(); id != 0 {
		t.Fatalf("possibly submitted attempts did not exhaust cap: %d", id)
	}
	p.now = p.now.Add(time.Hour)
	id = p.reserve()
	if id == 0 || !slices.Equal(p.begin(id), items[:OfferCap]) {
		t.Fatal("cap did not reset or a newer generation could not recover known non-submission")
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
}
