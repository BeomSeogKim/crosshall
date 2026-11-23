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
	visible := state
	visible.LastingLaunchSuspension = false
	if owner == nil || *owner != visible {
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

func TestExpiredPublishSuspensionsDoNotCount(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	for i := 1; i <= 3; i++ {
		p.observed("absorbed")
		if s := p.publishState(); s.State != "suspended" || s.UntilMS != p.now.Add(30*time.Minute).UnixMilli() {
			t.Fatalf("suspension %d: %+v, want a 30-minute suspension", i, s)
		}
		p.now = p.now.Add(30 * time.Minute)
	}
}

func TestPublishLastingSuspensionCountsOnlyQualifyingFailures(t *testing.T) {
	for _, test := range []struct {
		name, first, second, third string
		ownTurn, lasting           bool
	}{
		{"three absorbed", "absorbed", "absorbed", "absorbed", false, true},
		{"submission failure counts", "absorbed", "not_submitted", "absorbed", false, true},
		{"prior unknown is skipped", "unknown", "absorbed", "absorbed", false, false},
		{"latest unknown is timed", "absorbed", "absorbed", "unknown", false, false},
		{"own turn resets two failures", "absorbed", "absorbed", "absorbed", true, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := newPublishFixture(t)
			p.proveRoute()
			first := p.observed(test.first)
			p.now = p.now.Add(31 * time.Minute)
			var second int64
			if test.second == "not_submitted" {
				second = p.notSubmitted(3)
			} else {
				second = p.observed(test.second)
			}
			p.now = p.now.Add(31 * time.Minute)
			if test.ownTurn {
				p.turnEndAsk(p.writer, "reader", "own-turn")
				id := p.reserve()
				ids := p.begin(id)
				p.resolve(id, PublishSubmitted)
				p.hook(p.reader, p.promptReceipt("own-turn", publishTestLaunch, p.now.UnixMilli()+1, ids))
				p.observation(id, "own_turn")
			}
			p.turnEndAsk(p.writer, "reader", "third-failure")
			id := p.reserve()
			if id == 0 || len(p.begin(id)) == 0 {
				t.Fatal("no notice to suspend")
			}
			p.resolve(id, PublishSubmitted)
			p.mutate(`UPDATE publish_attempt SET observed_at_ms = ?, observation = ? WHERE id = ?`, p.now.UnixMilli(), test.third, id)
			// Stage overlapping suspension history after admission. The regular
			// queue would have blocked that admission while either was active.
			p.mutate(`UPDATE publish_suspension SET at_ms = ?, until_ms = ? WHERE notice IN (?, ?)`,
				p.now.UnixMilli(), p.now.Add(30*time.Minute).UnixMilli(), first, second)
			p.update(func(ctx context.Context, tx store.Tx) error { return suspendPublish(ctx, tx, id, test.third, p.now) })
			var lasting bool
			if err := p.db.QueryRow(`SELECT until_ms IS NULL FROM publish_suspension WHERE notice = ?`, id).Scan(&lasting); err != nil {
				t.Fatal(err)
			}
			if lasting != test.lasting {
				t.Fatalf("suspension lasts until launch ends: %t, want %t", lasting, test.lasting)
			}
			if test.lasting {
				p.now = p.now.Add(31 * time.Minute)
				p.turnEndAsk(p.writer, "reader", "later")
				p.requirePublish("the lasting suspension does not lift by time", "suspended", "absorbed")
				wantOK(t, p.owner(OwnerPublish{BindingID: p.binding.ID, Switch: "resume"}, "resume"))
				p.requirePublish("owner resume lifts it", "available", "")
			}
		})
	}
}

func TestLegacyUnknownSuspensionExpires(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	id := p.observed("unknown")
	p.mutate(`UPDATE publish_suspension SET until_ms = NULL WHERE notice = ?`, id)
	p.turnEndAsk(p.writer, "reader", "while-suspended")
	p.requirePublish("legacy unknown before its expiry", "suspended", "unknown")
	if s := p.publishState(); s.UntilMS != p.now.Add(30*time.Minute).UnixMilli() {
		t.Fatalf("legacy unknown: %+v, want a 30-minute suspension", s)
	}
	p.now = p.now.Add(30 * time.Minute)
	p.requirePublish("legacy unknown at expiry", "available", "")
}

func TestPublishManifestEvidenceHonorsExactOverride(t *testing.T) {
	for _, level := range []string{"candidate", "unavailable"} {
		t.Run(level, func(t *testing.T) {
			p := newPublishFixture(t)
			p.proveRoute()
			p.turnEndAsk(p.writer, "reader", "work")
			p.mutate(`UPDATE capability_evidence SET vendor_version = 'other' WHERE cap = 'publish'`)
			p.requirePublish("another version covers this manifest", "available", "trusted from")
			var e AdapterEvidence
			p.update(func(ctx context.Context, tx store.Tx) error {
				return tx.QueryRowContext(ctx, `SELECT json_extract(profile, '$.vendor_version'), json_extract(profile, '$.platform'),
					json_extract(profile, '$.mode'), json_extract(profile, '$.manifest_rev') FROM adapter_launch WHERE launch_ref = ?`,
					publishTestLaunch).Scan(&e.VendorVersion, &e.Platform, &e.Mode, &e.ManifestRev)
			})
			e.Vendor, e.RunAtMS, e.ResultRef = "claude", p.now.UnixMilli(), "sha256:01"
			e.Results = []EvidenceResult{{Cap: "publish", Level: level, Check: "exact version follow-up"}}
			wantOK(t, p.owner(e, "import-negative"))
			p.requirePublish("exact version overrides the manifest", "candidate", "exact evidence is "+level)
			wantOK(t, p.owner(OwnerPublish{BindingID: p.binding.ID, Switch: "resume"}, "resume"))
			p.requirePublish("resume leaves exact evidence in force", "candidate", "exact evidence is "+level)
			e.RunAtMS++
			e.ResultRef = "sha256:02"
			e.Results = []EvidenceResult{{Cap: "publish", Level: "tested", Check: "interactive conformance"}}
			wantOK(t, p.owner(e, "import-tested"))
			p.requirePublish("the imported evidence holds the version", "available", "")
		})
	}
}

func TestBuiltinEvidenceIsSeededAndOwnerRowsKept(t *testing.T) {
	p := newPublishFixture(t)
	shipped := []BuiltinEvidence{
		{Vendor: "claude", Version: "1", Platform: "p", Mode: "m", ManifestRev: "r", Capability: "publish", ResultRef: "sha256:aa", CheckedAtMS: 5},
		{Vendor: "claude", Version: "1", Platform: "p", Mode: "m", ManifestRev: "r", Capability: "turn_end", ResultRef: "sha256:aa", CheckedAtMS: 5},
	}
	seed := func(evidence []BuiltinEvidence) {
		p.update(func(ctx context.Context, tx store.Tx) error { return SeedBuiltinEvidence(ctx, tx, evidence) })
	}
	seed(shipped)
	p.now = p.now.Add(time.Hour)
	seed(shipped)
	if n := p.count(`SELECT COUNT(*) FROM capability_evidence WHERE source = 'builtin' AND level = 'tested'
		AND result_ref = 'sha256:aa' AND checked_at_ms = 5`); n != 2 {
		t.Fatalf("shipped rows with the run's own time: %d, want both after a restart", n)
	}

	// An owner import cannot claim to be shipped, even with the same
	// reference, and a newer import is not outranked by a restart.
	e := AdapterEvidence{Vendor: "claude", VendorVersion: "1", Platform: "p", Mode: "m", ManifestRev: "r",
		RunAtMS: 9, ResultRef: "builtin", Results: []EvidenceResult{{Cap: "publish", Level: "candidate", Check: "rerun"}}}
	wantOK(t, p.owner(e, "import"))
	e.VendorVersion = "2"
	wantOK(t, p.owner(e, "import-2"))
	seed(shipped)
	if n := p.count(`SELECT COUNT(*) FROM capability_evidence WHERE vendor_version = '1' AND source = 'owner'
		AND level = 'candidate' AND checked_at_ms = 9`); n != 1 {
		t.Fatalf("a restart outranked newer owner evidence: %d", n)
	}
	seed(nil)
	if n := p.count(`SELECT COUNT(*) FROM capability_evidence WHERE source = 'builtin'`); n != 0 {
		t.Fatalf("seeding an empty list left %d built-in rows", n)
	}
	if n := p.count(`SELECT COUNT(*) FROM capability_evidence WHERE vendor_version IN ('1', '2') AND result_ref = 'builtin'`); n != 2 {
		t.Fatalf("seeding removed owner rows that used the builtin reference: %d left, want 2", n)
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

func TestPublishWaitingShowsSubmissionObservationAndPickup(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	ask := decode[AskResult](t, p.ask(p.writer, Ask{To: []string{"reader"}, Body: "work"}, "work")).ID
	recipient := func() LedgerRecipient {
		t.Helper()
		for _, r := range decode[WaitingResult](t, p.run(p.writer, Waiting{}, "")).Requests {
			if r.ID == ask {
				return r.Recipients[0]
			}
		}
		t.Fatalf("#%d not waiting", ask)
		return LedgerRecipient{}
	}
	if n := recipient().Notice; n != nil {
		t.Fatalf("notice before any attempt: %+v", n)
	}
	fence := p.now.UnixMilli()
	id := p.reserve()
	p.begin(id)
	p.now = p.now.Add(time.Second)
	p.resolve(id, PublishNotSubmitted)
	if n := recipient().Notice; n == nil || n.ID != id || n.State != "not_submitted" {
		t.Fatalf("a released claim lost its notice: %+v", n)
	}
	// A known non-submission is retried only on a newer generation.
	p.mutate(`UPDATE publish_route SET generation = generation + 1 WHERE binding_id = ?`, p.binding.ID)
	p.proveRoute()
	id = p.reserve()
	p.begin(id)
	p.now = p.now.Add(time.Second)
	p.resolve(id, PublishSubmitted)
	n := recipient().Notice
	if n == nil || n.ID != id || n.State != "submitted" || n.ResolvedAtMS == nil || *n.ResolvedAtMS != p.now.UnixMilli() ||
		n.SubmittingAtMS == nil || *n.SubmittingAtMS == *n.ResolvedAtMS || n.ObservedAtMS != nil {
		t.Fatalf("submitted notice: %+v (fence at %d)", n, fence)
	}
	p.mutate(`UPDATE publish_attempt SET observed_at_ms = ?, observation = 'own_turn' WHERE id = ?`, p.now.UnixMilli(), id)
	p.pickUp(p.reader, ask)
	if n := recipient().Notice; n == nil || n.Observation != "own_turn" || n.ObservedAtMS == nil {
		t.Fatalf("observed notice: %+v", n)
	}

	// The reader's final reply is offered to the writer, who waits on it.
	writer := bindingIDOf(t, p.fixture, p.principalOf(p.writer))
	wantOK(t, p.reply(p.reader, Reply{To: ask, Body: "done"}, "answer"))
	p.mutate(`INSERT INTO publish_attempt (binding_id, principal_id, binding_epoch, generation, adapter, session_ref, launch_ref,
			policy_epoch, process_pid, process_start_ms, state, reserved_at_ms, submitting_at_ms, resolved_at_ms)
		SELECT b.id, b.principal_id, 1, 1, 'codex', 's', 'l', 1, 77, 1, 'submitted', ?1, ?1, ?1 FROM binding b WHERE b.id = ?2`,
		p.now.UnixMilli(), writer)
	p.mutate(`INSERT INTO publish_attempt_item (attempt_id, principal_id, message_id)
		SELECT (SELECT MAX(id) FROM publish_attempt), m.sender_id, r.id FROM message m JOIN message r ON r.in_reply_to = m.id
		WHERE m.id = ?`, ask)
	if n := recipient().ReplyNotice; n == nil || n.State != "submitted" {
		t.Fatalf("the reply's notice to the asker is missing: %+v", n)
	}

	pull := decode[AskResult](t, p.ask(p.reader, Ask{To: []string{"writer"}, Body: "pull"}, "pull")).ID
	for _, r := range decode[WaitingResult](t, p.run(p.reader, Waiting{}, "")).Requests {
		if r.ID == pull && (r.Recipients[0].Notice != nil || r.Recipients[0].ReplyNotice != nil) {
			t.Fatal("a member on pull gained a notice line")
		}
	}
}

func TestPublishHeldFollowsThePhysicalQueueAcrossBindings(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	other := p.newSession("claude")
	wantOK(t, p.join(other, Join{Channel: "checkout", Alias: "other"}, "other-join"))
	enrollAndRoute(t, p.fixture, other, publishTestLaunch, "startup", publishTestSocket, "default", p.now.UnixMilli(), 1)
	b := PublishBinding{ID: bindingIDOf(t, p.fixture, p.principalOf(other))}
	b.Generation, _, _, _, _, _, _ = routeState(t, p.fixture, b.ID)
	p.update(func(ctx context.Context, tx store.Tx) error { return ConfirmPublishRoute(ctx, tx, b, p.queue) })
	p.turnEndAsk(p.writer, "reader", "first")
	id := p.reserve()
	if id == 0 {
		t.Fatal("no reservation")
	}
	var state store.PublishState
	p.update(func(ctx context.Context, tx store.Tx) error {
		var err error
		state, _, err = store.ReadPublishState(ctx, tx, b.ID, p.now)
		return err
	})
	if state.State != "held" || state.Notice != id {
		t.Fatalf("the other binding on the same queue shows %+v while reserved attempt %d holds it", state, id)
	}
}

func TestPublishOwnerHardStopIsReportedOverTheEndedRoute(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	disabled := false
	wantOK(t, p.owner(AdapterSet{AdapterPolicyArgs: wire.AdapterPolicyArgs{Adapter: "claude", Enabled: &disabled}}, "disable"))
	if s := p.publishState(); s.State != "off" || !strings.Contains(s.Reason, "disabled") {
		t.Fatalf("adapter disabled shows %+v", s)
	}
}

func TestPublishResumeAndSuspensionInOneMillisecond(t *testing.T) {
	t.Run("suspension then resume", func(t *testing.T) {
		p := newPublishFixture(t)
		p.proveRoute()
		p.observed("absorbed")
		wantOK(t, p.owner(OwnerPublish{BindingID: p.binding.ID, Switch: "resume"}, "resume"))
		p.turnEndAsk(p.writer, "reader", "after")
		p.requirePublish("resume committed after the suspension", "available", "")
	})
	t.Run("resume then suspension", func(t *testing.T) {
		p := newPublishFixture(t)
		p.proveRoute()
		wantOK(t, p.owner(OwnerPublish{BindingID: p.binding.ID, Switch: "resume"}, "resume"))
		p.observed("absorbed")
		p.requirePublish("suspension committed after the resume", "suspended", "absorbed")
	})
}

func TestPublishSendResultIsPullUnlessThisMessageIsQueued(t *testing.T) {
	p := newPublishFixture(t)
	const pull = "waits for its next crosshall command"
	send := func(key string) Delivery {
		t.Helper()
		return decode[AskResult](t, p.ask(p.writer, Ask{To: []string{"reader"}, Body: "work"}, key)).Recipients[0]
	}
	if d := send("candidate"); d.Next != pull {
		t.Fatalf("candidate route: %+v", d)
	}
	p.proveRoute()
	if d := send("candidate"); d.Next != pull {
		t.Fatalf("replay of a message below the activation watermark: %+v", d)
	}
	if d := send("queued"); d.Next != "notice queued" {
		t.Fatalf("available route, eligible message: %+v", d)
	}
	id := p.reserve()
	p.begin(id)
	p.resolve(id, PublishSubmitted)
	if d := send("queued"); d.Next != pull {
		t.Fatalf("replay after the notice was submitted: %+v", d)
	}
	wantOK(t, p.run(p.reader, Publish{}, "off"))
	if d := send("off"); d.Next != pull {
		t.Fatalf("member off: %+v", d)
	}
	note := decode[NoteResult](t, p.run(p.writer, Note{To: []string{"reader"}, Body: "fyi"}, "note"))
	if d := note.Recipients[0]; d.Next != pull {
		t.Fatalf("a note: %+v", d)
	}
}

func TestPublishSuspendsOnlyOnThreeNonSubmissionsInARow(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	p.notSubmitted(1)
	p.now = p.now.Add(time.Millisecond)
	p.turnEndAsk(p.writer, "reader", "success")
	id := p.reserve()
	if id == 0 || len(p.begin(id)) == 0 {
		t.Fatal("no notice to submit")
	}
	p.resolve(id, PublishSubmitted)
	p.mutate(`UPDATE publish_attempt SET observed_at_ms = ?, observation = 'own_turn' WHERE id = ?`, p.now.UnixMilli(), id)
	p.notSubmitted(2)
	p.turnEndAsk(p.writer, "reader", "after-fail-success-fail-fail")
	p.requirePublish("fail, success, fail, fail", "available", "")
	p.notSubmitted(1)
	p.turnEndAsk(p.writer, "reader", "after-three-in-a-row")
	p.requirePublish("three in a row", "suspended", "the third in a row")
}

func TestPublishSendResultIsPullOnAHeldOrCappedRoute(t *testing.T) {
	const pull = "waits for its next crosshall command"
	send := func(p *publishFixture, key string) Delivery {
		t.Helper()
		return decode[AskResult](t, p.ask(p.writer, Ask{To: []string{"reader"}, Body: "work"}, key)).Recipients[0]
	}
	t.Run("held", func(t *testing.T) {
		p := newPublishFixture(t)
		p.proveRoute()
		p.turnEndAsk(p.writer, "reader", "first")
		id := p.reserve()
		p.begin(id)
		p.resolve(id, PublishSubmitted)
		if s := p.publishState(); s.State != "held" {
			t.Fatalf("state %+v, want held", s)
		}
		if d := send(p, "while-held"); d.Next != pull {
			t.Fatalf("a held route said %q", d.Next)
		}
	})
	t.Run("capped", func(t *testing.T) {
		p := newPublishFixture(t)
		p.proveRoute()
		p.mutate(`WITH RECURSIVE seq(n) AS (SELECT 1 UNION ALL SELECT n + 1 FROM seq WHERE n < ?)
			INSERT INTO publish_attempt (binding_id, principal_id, binding_epoch, generation, adapter, session_ref, launch_ref,
				policy_epoch, process_pid, process_start_ms, state, reserved_at_ms, submitting_at_ms, resolved_at_ms, observed_at_ms, observation)
			SELECT binding_id, principal_id, binding_epoch, generation, adapter, session_ref, launch_ref,
				policy_epoch, 42, ?, 'submitted', ?, ?, ?, ?, 'own_turn'
			FROM publish_route CROSS JOIN seq WHERE binding_id = ?`,
			store.PublishHourlyCap, p.queue.StartMS, p.now.UnixMilli(), p.now.UnixMilli(), p.now.UnixMilli(), p.now.UnixMilli(), p.binding.ID)
		if s := p.publishState(); s.State != "capped" {
			t.Fatalf("state %+v, want capped", s)
		}
		if d := send(p, "while-capped"); d.Next != pull {
			t.Fatalf("a capped route said %q", d.Next)
		}
	})
}

func TestPublishResumeCountsAnOutstandingReservationByItsOutcome(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	p.turnEndAsk(p.writer, "reader", "a")
	a := p.reserve()
	if a == 0 || len(p.begin(a)) == 0 {
		t.Fatal("no notice A")
	}
	wantOK(t, p.owner(OwnerPublish{BindingID: p.binding.ID, Switch: "resume"}, "resume"))
	p.now = p.now.Add(time.Millisecond)
	p.resolve(a, PublishNotSubmitted) // reserved before the resume, failed after it
	p.notSubmitted(2)
	p.turnEndAsk(p.writer, "reader", "after")
	p.requirePublish("A, B and C failed after the resume", "suspended", "the third in a row")
}
