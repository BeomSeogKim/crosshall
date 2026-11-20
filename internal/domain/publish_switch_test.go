package domain

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func (p *publishFixture) publishState() store.PublishState {
	p.t.Helper()
	var state store.PublishState
	p.update(func(ctx context.Context, tx store.Tx) error {
		var err error
		state, _, err = store.ReadPublishState(ctx, tx, p.binding.ID, p.now)
		return err
	})
	if who := p.memberRow(p.writer, "reader").Publish; who == nil || *who != (store.PublishState{State: state.State}) {
		p.t.Fatalf("an agent's who shows %+v, want only the word %q", who, state.State)
	}
	var owner *store.PublishState
	for _, ch := range decode[OwnerWhoResult](p.t, p.owner(OwnerWho{}, "")).Channels {
		for _, m := range ch.Members {
			if m.BindingID == p.binding.ID {
				owner = m.Publish
			}
		}
	}
	if owner == nil || *owner != state {
		p.t.Fatalf("owner who shows %+v, the store %+v", owner, state)
	}
	return state
}

func (p *publishFixture) hasWork() bool {
	p.t.Helper()
	var pending bool
	p.update(func(ctx context.Context, tx store.Tx) error {
		var err error
		pending, err = HasPublishWork(ctx, tx, p.binding, p.now)
		return err
	})
	return pending
}

// requirePublish checks the eligibility the publisher uses and the state
// who shows together, so the two cannot disagree.
func (p *publishFixture) requirePublish(step, state, reason string) {
	p.t.Helper()
	got := p.publishState()
	if got.State != state || !strings.Contains(got.Reason, reason) {
		p.t.Fatalf("%s: publish %+v, want %s (%s)", step, got, state, reason)
	}
	if pending := p.hasWork(); pending != (state == "available") {
		p.t.Fatalf("%s: eligibility %t disagrees with state %s", step, pending, state)
	}
}

func TestPublishSwitchLevelsNarrowestOwnerWins(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	p.turnEndAsk(p.writer, "reader", "work")
	member := func(sw, key string) {
		t.Helper()
		wantOK(t, p.owner(OwnerPublish{BindingID: p.binding.ID, Switch: sw}, key))
	}
	self := func(on bool, key string) {
		t.Helper()
		wantOK(t, p.run(p.reader, Publish{On: on}, key))
	}
	p.requirePublish("no switch set", "available", "")

	wantOK(t, p.owner(OwnerPublish{Hall: true, Switch: "off"}, "hall-off"))
	p.requirePublish("hall off", "off", "for the hall")
	member("on", "member-on")
	p.requirePublish("member on beats hall off", "available", "")
	member("default", "member-default")
	p.requirePublish("member default falls back to the hall", "off", "for the hall")

	p.mutate(`UPDATE adapter_policy SET publish = 1 WHERE adapter = 'claude'`)
	p.requirePublish("adapter on beats hall off", "available", "")
	member("off", "member-off")
	p.requirePublish("member off beats adapter on", "off", "for this member")
	member("default", "member-default-2")
	wantOK(t, p.owner(OwnerPublish{Hall: true, Switch: "on"}, "hall-on"))
	p.mutate(`UPDATE adapter_policy SET publish = 0 WHERE adapter = 'claude'`)
	p.requirePublish("adapter off beats hall on", "off", "for claude")
	p.mutate(`UPDATE adapter_policy SET publish = NULL WHERE adapter = 'claude'`)
	p.requirePublish("every owner switch on", "available", "")

	self(false, "self-off")
	p.requirePublish("member's own off", "off", "by the member")
	member("on", "member-on-2")
	p.requirePublish("an owner on does not override the member's own off", "off", "by the member")
	self(true, "self-on")
	p.requirePublish("member's own on", "available", "")
	member("off", "member-off-2")
	self(true, "self-on-again")
	p.requirePublish("the member's on never overrides an owner off", "off", "for this member")

	member("on", "member-on-3")
	p.mutate(`UPDATE adapter_policy SET enabled = 0 WHERE adapter = 'claude'`)
	p.requirePublish("adapter disabled is a hard stop", "off", "disabled")
}

func TestPublishSwitchCommandsReplayAndRefuseAgents(t *testing.T) {
	p := newPublishFixture(t)
	first := decode[PublishSwitchResult](t, p.run(p.reader, Publish{}, "quiet"))
	if !first.Changed || first.Switch != "off" || first.Publish == nil || first.Publish.State != "off" {
		t.Fatalf("member off: %+v", first)
	}
	if again := decode[PublishSwitchResult](t, p.run(p.reader, Publish{}, "quiet")); again.Changed != first.Changed {
		t.Fatalf("replay changed the result: %+v", again)
	}
	if next := decode[PublishSwitchResult](t, p.run(p.reader, Publish{}, "quiet-2")); next.Changed {
		t.Fatalf("a second off changed the switch: %+v", next)
	}
	wantCode(t, p.run(p.reader, OwnerPublish{Hall: true, Switch: "off"}, "agent-hall"), 3, "owner_only")
	if n := p.count(`SELECT COUNT(*) FROM hall_publish`); n != 0 {
		t.Fatalf("an agent changed the hall switch")
	}
	wantCode(t, p.owner(OwnerPublish{BindingID: 999, Switch: "off"}, "missing"), 2, "")
	wantCode(t, p.owner(OwnerPublish{Hall: true, Switch: "default"}, "hall-default"), 2, "")
}

func TestPublishAdapterSwitchKeepsTheEpochAndTheRoute(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	p.turnEndAsk(p.writer, "reader", "work")
	off := "off"
	policy := decode[store.AdapterPolicy](t, p.owner(AdapterSet{AdapterPolicyArgs: wire.AdapterPolicyArgs{Adapter: "claude", Publish: &off}}, "vendor-off"))
	if policy.Publish != "off" || policy.Epoch != 1 {
		t.Fatalf("policy after --publish off: %+v", policy)
	}
	p.requirePublish("adapter publish off", "off", "for claude")
	def := "default"
	policy = decode[store.AdapterPolicy](t, p.owner(AdapterSet{AdapterPolicyArgs: wire.AdapterPolicyArgs{Adapter: "claude", Publish: &def}}, "vendor-default"))
	if policy.Publish != "" || policy.Epoch != 1 {
		t.Fatalf("policy after --publish default: %+v", policy)
	}
	p.requirePublish("the same route publishes again without a new session", "available", "")
}

func TestPublishOffStopsAReservedAttemptBeforeTheFence(t *testing.T) {
	for _, test := range []struct {
		name string
		off  func(p *publishFixture)
	}{
		{"member", func(p *publishFixture) { wantOK(p.t, p.run(p.reader, Publish{}, "off")) }},
		{"owner member", func(p *publishFixture) {
			wantOK(p.t, p.owner(OwnerPublish{BindingID: p.binding.ID, Switch: "off"}, "off"))
		}},
		{"hall", func(p *publishFixture) { wantOK(p.t, p.owner(OwnerPublish{Hall: true, Switch: "off"}, "off")) }},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := newPublishFixture(t)
			p.proveRoute()
			ask := p.turnEndAsk(p.writer, "reader", "work")
			id := p.reserve()
			if id == 0 {
				t.Fatal("no reservation")
			}
			test.off(p)
			if ids := p.begin(id); len(ids) != 0 {
				t.Fatalf("an off switch let attempt %d past the fence with %v", id, ids)
			}
			p.state(id, "not_submitted")
			if n := p.count(`SELECT COUNT(*) FROM announcement WHERE message_id = ?`, ask); n != 0 {
				t.Fatal("the stopped attempt kept its claim")
			}
			if s := p.publishState(); s.State != "off" {
				t.Fatalf("publish %+v, want off", s)
			}
		})
	}
}

// observed submits one notice and records how its prompt was observed.
func (p *publishFixture) observed(observation string) int64 {
	p.t.Helper()
	p.turnEndAsk(p.writer, "reader", fmt.Sprintf("work-%d", p.now.UnixNano()))
	id := p.reserve()
	if id == 0 || len(p.begin(id)) == 0 {
		p.t.Fatal("no notice to observe")
	}
	p.resolve(id, PublishSubmitted)
	p.mutate(`UPDATE publish_attempt SET observed_at_ms = ?, observation = ? WHERE id = ?`, p.now.UnixMilli(), observation, id)
	p.update(func(ctx context.Context, tx store.Tx) error { return suspendPublish(ctx, tx, id, observation, p.now) })
	return id
}

// notSubmitted has the transport fail to submit n notices in a row.
func (p *publishFixture) notSubmitted(n int) int64 {
	p.t.Helper()
	var id int64
	for range n {
		p.now = p.now.Add(time.Millisecond)
		p.turnEndAsk(p.writer, "reader", fmt.Sprintf("fail-%d", p.now.UnixNano()))
		if id = p.reserve(); id == 0 || len(p.begin(id)) == 0 {
			p.t.Fatal("no notice to submit")
		}
		p.resolve(id, PublishNotSubmitted)
	}
	return id
}

func TestPublishSuspensionReasonsAndHowTheyLift(t *testing.T) {
	newLaunch := func(p *publishFixture) {
		p.t.Helper()
		p.now = p.now.Add(time.Minute)
		enrollAndRoute(p.t, p.fixture, p.reader, "pid-43-start", "resume", publishTestSocket, "default", p.now.UnixMilli(), 1)
		p.proveRoute()
		p.turnEndAsk(p.writer, "reader", "after-activation") // older work stays below the new watermark
	}
	for _, test := range []struct {
		name, reason string
		suspend      func(p *publishFixture) int64
	}{
		{"absorbed", "absorbed", func(p *publishFixture) int64 { return p.observed("absorbed") }},
		{"unknown", "unknown", func(p *publishFixture) int64 { return p.observed("unknown") }},
		{"three not submitted", "not submitted, the third in a row", func(p *publishFixture) int64 {
			p.notSubmitted(2)
			p.turnEndAsk(p.writer, "reader", "fenced")
			id := p.reserve()
			wantOK(t, p.run(p.reader, Publish{}, "fence-off"))
			p.begin(id) // stopped at the fence: does not count
			wantOK(t, p.run(p.reader, Publish{On: true}, "fence-on"))
			p.turnEndAsk(p.writer, "reader", "after-fence")
			p.requirePublish("two failures and a fence stop", "available", "")
			return p.notSubmitted(1)
		}},
	} {
		for _, by := range []string{"time", "launch end", "resume"} {
			t.Run(test.name+" lifted by "+by, func(t *testing.T) {
				p := newPublishFixture(t)
				p.proveRoute()
				notice := test.suspend(p)
				p.turnEndAsk(p.writer, "reader", "while-suspended")
				p.requirePublish("suspended", "suspended", test.reason)
				s := p.publishState()
				if s.Notice != notice || s.AtMS != p.now.UnixMilli() || s.UntilMS != p.now.Add(30*time.Minute).UnixMilli() || s.Lift == "" {
					t.Fatalf("suspension %+v, want notice %d until 30 minutes on", s, notice)
				}
				switch by {
				case "time":
					p.now = p.now.Add(29 * time.Minute)
					p.requirePublish("before the lift time", "suspended", test.reason)
					p.now = p.now.Add(time.Minute)
				case "launch end":
					newLaunch(p)
				case "resume":
					p.now = p.now.Add(time.Second)
					wantOK(t, p.owner(OwnerPublish{BindingID: p.binding.ID, Switch: "resume"}, "resume"))
				}
				p.requirePublish("lifted by "+by, "available", "")
			})
		}
	}
}

func TestPublishThirdSuspensionLastsForTheLaunch(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	for i := 1; i <= 3; i++ {
		p.observed("absorbed")
		s := p.publishState()
		if lasting := s.UntilMS == 0; s.State != "suspended" || lasting != (i == 3) {
			t.Fatalf("suspension %d: %+v", i, s)
		}
		p.now = p.now.Add(31 * time.Minute)
	}
	p.turnEndAsk(p.writer, "reader", "later")
	p.requirePublish("the third suspension does not lift by time", "suspended", "absorbed")
	p.now = p.now.Add(time.Second)
	wantOK(t, p.owner(OwnerPublish{BindingID: p.binding.ID, Switch: "resume"}, "resume"))
	p.requirePublish("resume lifts it", "available", "")
	p.now = p.now.Add(time.Millisecond)
	p.observed("absorbed")
	if s := p.publishState(); s.UntilMS == 0 {
		t.Fatalf("resume did not restart the count: %+v", s)
	}
}

func TestPublishVersionSuspension(t *testing.T) {
	for _, by := range []string{"profile list", "resume"} {
		t.Run(by, func(t *testing.T) {
			p := newPublishFixture(t)
			p.proveRoute()
			p.turnEndAsk(p.writer, "reader", "work")
			p.mutate(`UPDATE capability_evidence SET vendor_version = 'other' WHERE cap = 'publish'`)
			p.requirePublish("version not listed", "suspended", `version "`)
			if by == "resume" {
				wantOK(t, p.owner(OwnerPublish{BindingID: p.binding.ID, Switch: "resume"}, "resume"))
				p.requirePublish("resume does not add the version", "suspended", `version "`)
				return
			}
			p.update(func(ctx context.Context, tx store.Tx) error {
				var profile PublishProfile
				err := tx.QueryRowContext(ctx, `SELECT json_extract(profile, '$.vendor_version'), json_extract(profile, '$.platform'),
					json_extract(profile, '$.mode'), json_extract(profile, '$.manifest_rev') FROM adapter_launch WHERE launch_ref = ?`,
					publishTestLaunch).Scan(&profile.Version, &profile.Platform, &profile.Mode, &profile.ManifestRev)
				if err != nil {
					return err
				}
				profile.Vendor = "claude"
				return SeedPublishProfiles(ctx, tx, []PublishProfile{profile}, p.now)
			})
			p.requirePublish("the shipped list holds the version", "available", "")
		})
	}
}

func TestPublishShippedProfilesAreSeededAndOwnerRowsKept(t *testing.T) {
	p := newPublishFixture(t)
	shipped := []PublishProfile{{Vendor: "claude", Version: "1", Platform: "p", Mode: "m", ManifestRev: "r"}}
	seed := func(profiles []PublishProfile) {
		p.update(func(ctx context.Context, tx store.Tx) error { return SeedPublishProfiles(ctx, tx, profiles, p.now) })
	}
	seed(shipped)
	seed(shipped)
	if n := p.count(`SELECT COUNT(*) FROM capability_evidence WHERE result_ref = 'builtin' AND level = 'tested'`); n != 1 {
		t.Fatalf("shipped rows %d, want 1", n)
	}
	seed(nil)
	if n := p.count(`SELECT COUNT(*) FROM capability_evidence WHERE result_ref = 'builtin'`); n != 0 {
		t.Fatalf("a profile the binary no longer ships stayed: %d", n)
	}
	if n := p.count(`SELECT COUNT(*) FROM capability_evidence WHERE cap = 'publish' AND result_ref = 'publish-test'`); n != 1 {
		t.Fatalf("seeding removed an imported row: %d", n)
	}
}

func TestPublishHeldAndCappedStates(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	p.turnEndAsk(p.writer, "reader", "work")
	id := p.reserve()
	p.begin(id)
	p.resolve(id, PublishSubmitted)
	held := p.publishState()
	if held.State != "held" || held.Notice != id || held.AtMS != p.now.UnixMilli() {
		t.Fatalf("held state %+v, want notice %d", held, id)
	}
	p.mutate(`UPDATE publish_attempt SET observed_at_ms = ?, observation = 'own_turn' WHERE id = ?`, p.now.UnixMilli(), id)
	p.turnEndAsk(p.writer, "reader", "more")
	p.mutate(`WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < ?)
		INSERT INTO publish_attempt (binding_id, principal_id, binding_epoch, generation, adapter, session_ref, launch_ref,
			policy_epoch, process_pid, process_start_ms, state, reserved_at_ms, submitting_at_ms, resolved_at_ms, observed_at_ms, observation)
		SELECT binding_id, principal_id, binding_epoch, generation, adapter, session_ref, launch_ref,
			policy_epoch, 42, ?, 'submitted', ?, ?, ?, ?, 'own_turn'
		FROM publish_route CROSS JOIN seq WHERE binding_id = ?`,
		store.PublishHourlyCap, p.queue.StartMS, p.now.UnixMilli(), p.now.UnixMilli(), p.now.UnixMilli(), p.now.UnixMilli(), p.binding.ID)
	capped := p.publishState()
	if capped.State != "capped" || capped.UntilMS != p.now.Add(time.Hour).UnixMilli() {
		t.Fatalf("capped state %+v", capped)
	}
	p.now = p.now.Add(time.Hour)
	p.requirePublish("after the cap window", "available", "")
}

func TestPublishSendResultSaysWhetherAPublishIsPending(t *testing.T) {
	p := newPublishFixture(t)
	next := func(key string) Delivery {
		t.Helper()
		r := decode[AskResult](t, p.ask(p.writer, Ask{To: []string{"reader"}, Body: "work"}, key))
		return r.Recipients[0]
	}
	if d := next("candidate"); d.Next != "waits for its next crosshall command" || d.Publish == nil ||
		*d.Publish != (PublishStatus{State: "candidate"}) {
		t.Fatalf("candidate route: %+v", d)
	}
	p.proveRoute()
	if d := next("available"); d.Next != "publish pending" || d.Publish != nil {
		t.Fatalf("available route: %+v", d)
	}
	wantOK(t, p.run(p.reader, Publish{}, "off"))
	if d := next("off"); d.Next != "waits for its next crosshall command" || d.Publish == nil || d.Publish.State != "off" {
		t.Fatalf("member off: %+v", d)
	}
	note := decode[NoteResult](t, p.run(p.writer, Note{To: []string{"reader"}, Body: "fyi"}, "note"))
	if d := note.Recipients[0]; d.Publish != nil {
		t.Fatalf("a note carried a publish reason: %+v", d)
	}
	pull := decode[AskResult](t, p.ask(p.reader, Ask{To: []string{"writer"}, Body: "pull"}, "pull"))
	if d := pull.Recipients[0]; d.Next != "waits for its next crosshall command" || d.Publish != nil {
		t.Fatalf("a member without a route: %+v", d)
	}
}

func TestPublishWaitingShowsSubmissionObservationAndPickup(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	ask := decode[AskResult](t, p.ask(p.writer, Ask{To: []string{"reader"}, Body: "work"}, "work")).ID
	notice := func() *LedgerNotice {
		t.Helper()
		for _, r := range decode[WaitingResult](t, p.run(p.writer, Waiting{}, "")).Requests {
			if r.ID == ask {
				return r.Recipients[0].Notice
			}
		}
		t.Fatalf("#%d not waiting", ask)
		return nil
	}
	if n := notice(); n != nil {
		t.Fatalf("notice before any attempt: %+v", n)
	}
	id := p.reserve()
	p.begin(id)
	p.resolve(id, PublishSubmitted)
	if n := notice(); n == nil || n.ID != id || n.State != "submitted" || n.SubmittedAtMS == nil || n.ObservedAtMS != nil {
		t.Fatalf("submitted notice: %+v", n)
	}
	p.mutate(`UPDATE publish_attempt SET observed_at_ms = ?, observation = 'own_turn' WHERE id = ?`, p.now.UnixMilli(), id)
	p.pickUp(p.reader, ask)
	n := notice()
	if n == nil || n.Observation != "own_turn" || n.ObservedAtMS == nil {
		t.Fatalf("observed notice: %+v", n)
	}
	pull := decode[AskResult](t, p.ask(p.reader, Ask{To: []string{"writer"}, Body: "pull"}, "pull")).ID
	for _, r := range decode[WaitingResult](t, p.run(p.reader, Waiting{}, "")).Requests {
		if r.ID == pull && r.Recipients[0].Notice != nil {
			t.Fatal("a member on pull gained a notice line")
		}
	}
}
