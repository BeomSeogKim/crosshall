package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

// lastingSuspension suspends the reader's notices for the rest of its
// launch with a third absorbed notice, and returns that notice. The first
// two suspensions are restaged as still in force, since the queue would
// not have admitted the third notice while either was.
func (p *publishFixture) lastingSuspension() int64 {
	p.t.Helper()
	first := p.observed("absorbed")
	p.now = p.now.Add(31 * time.Minute)
	second := p.observed("absorbed")
	p.now = p.now.Add(31 * time.Minute)
	p.turnEndAsk(p.writer, "reader", fmt.Sprintf("third-%d", p.now.UnixNano()))
	id := p.reserve()
	if id == 0 || len(p.begin(id)) == 0 {
		p.t.Fatal("no notice to suspend")
	}
	p.resolve(id, PublishSubmitted)
	p.mutate(`UPDATE publish_attempt SET observed_at_ms = ?, observation = 'absorbed' WHERE id = ?`, p.now.UnixMilli(), id)
	p.mutate(`UPDATE publish_suspension SET at_ms = ?, until_ms = ? WHERE notice IN (?, ?)`,
		p.now.UnixMilli(), p.now.Add(30*time.Minute).UnixMilli(), first, second)
	p.update(func(ctx context.Context, tx store.Tx) error { return suspendPublish(ctx, tx, id, "absorbed", p.now) })
	var lasting bool
	if err := p.db.QueryRow(`SELECT until_ms IS NULL FROM publish_suspension WHERE notice = ?`, id).Scan(&lasting); err != nil || !lasting {
		p.t.Fatalf("the third suspension is not lasting (%v)", err)
	}
	return id
}

type suspensionEvent struct {
	subject string
	actor   int64
	detail  publishSuspended
	alerted bool
}

func (p *publishFixture) suspensionEvents() []suspensionEvent {
	p.t.Helper()
	rows, err := p.db.Query(`SELECT e.subject, e.actor_id, e.detail, EXISTS (SELECT 1 FROM owner_alert a WHERE a.event_seq = e.seq)
		FROM event e WHERE e.kind = ? ORDER BY e.seq`, publishSuspendedEvent)
	if err != nil {
		p.t.Fatal(err)
	}
	defer rows.Close()
	var out []suspensionEvent
	for rows.Next() {
		var e suspensionEvent
		var raw string
		if err := rows.Scan(&e.subject, &e.actor, &raw, &e.alerted); err != nil {
			p.t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(raw), &e.detail); err != nil {
			p.t.Fatal(err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		p.t.Fatal(err)
	}
	return out
}

// Every suspension leaves an audit event; only a lasting one is an owner
// alert.
func TestEverySuspensionIsAuditedAndOnlyALastingOneAlerts(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	unknownAt := p.now
	unknown := p.observed("unknown")
	p.now = p.now.Add(31 * time.Minute)
	lasting := p.lastingSuspension()

	events := p.suspensionEvents()
	if len(events) != 4 {
		t.Fatalf("%d suspension events, want 4: %+v", len(events), events)
	}
	subject, reader := fmt.Sprintf("binding:%d", p.binding.ID), p.principalOf(p.reader)
	for i, e := range events {
		if e.subject != subject || e.actor != reader || e.detail.Channel != "checkout" || e.detail.Alias != "reader" ||
			e.detail.BindingID != p.binding.ID || e.detail.Suspension == 0 || e.alerted != (i == 3) {
			t.Fatalf("event %d: %+v", i, e)
		}
	}
	if d := events[0].detail; d.Reason != "unknown" || d.Notice != unknown || d.Lasting ||
		d.UntilMS != unknownAt.Add(30*time.Minute).UnixMilli() {
		t.Fatalf("the unknown suspension: %+v", d)
	}
	if d := events[3].detail; d.Reason != "absorbed" || d.Notice != lasting || !d.Lasting || d.UntilMS != 0 {
		t.Fatalf("the lasting suspension: %+v", d)
	}
}

func TestLastingSuspensionReachesTheOwnerOnce(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	notice := p.lastingSuspension()
	at := clockAt(p.now.UnixMilli())
	if r := p.run(p.reader, Status{}, ""); r.OwnerAlerts != nil {
		t.Fatalf("a member command carried the owner line: %q", r.OwnerAlerts)
	}
	want := []string{
		fmt.Sprintf("crosshall: notices to @reader in #checkout are off until its launch ends (third suspension, notice %d absorbed at %s).", notice, at),
		fmt.Sprintf("next: crosshall owner publish resume %d turns them on again.", p.binding.ID),
	}
	if got := p.owner(OwnerStatus{}, "").OwnerAlerts; !reflect.DeepEqual(got, want) {
		t.Fatalf("first owner command: %q, want %q", got, want)
	}
	if got := p.owner(OwnerStatus{}, "").OwnerAlerts; got != nil {
		t.Fatalf("the suspension was shown twice: %q", got)
	}

	var alerts []DesktopAlert
	p.update(func(ctx context.Context, tx store.Tx) (err error) {
		alerts, err = ClaimDesktopAlerts(ctx, tx, p.now, true)
		return err
	})
	wantAlert := DesktopAlert{Title: "crosshall: notices off in #checkout",
		Body: fmt.Sprintf("Notices to @reader stay off until its launch ends: notice %d absorbed, the third suspension. Run: crosshall owner publish resume %d",
			notice, p.binding.ID)}
	if len(alerts) != 1 || alerts[0].Title != wantAlert.Title || alerts[0].Body != wantAlert.Body {
		t.Fatalf("desktop alerts %+v, want %+v", alerts, wantAlert)
	}
}

// A suspension the owner already lifted is not reported, neither by the
// resume itself nor on the desktop.
func TestALiftedSuspensionIsNotReported(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	p.lastingSuspension()
	p.now = p.now.Add(time.Second)
	if r := p.owner(OwnerPublish{BindingID: p.binding.ID, Switch: "resume"}, "resume"); r.OwnerAlerts != nil {
		t.Fatalf("the resume reported the suspension it lifted: %q", r.OwnerAlerts)
	}
	var alerts []DesktopAlert
	p.update(func(ctx context.Context, tx store.Tx) (err error) {
		alerts, err = ClaimDesktopAlerts(ctx, tx, p.now, true)
		return err
	})
	if len(alerts) != 0 {
		t.Fatalf("a lifted suspension was shown: %+v", alerts)
	}
	if n := p.count(`SELECT COUNT(*) FROM owner_alert WHERE desktop_state = 'skipped'`); n != 1 {
		t.Fatalf("skipped desktop alerts: %d", n)
	}
}

func TestMoreAlertsLineNamesTheViewsThatShowThem(t *testing.T) {
	for _, tc := range []struct {
		stops, suspensions int
		want               string
	}{
		{2, 0, "and 2 more: crosshall owner status"},
		{0, 1, "and 1 more: crosshall owner who --all"},
		{1, 2, "and 3 more: crosshall owner status and crosshall owner who --all"},
	} {
		if got := moreAlertsLine(tc.stops, tc.suspensions); got != tc.want {
			t.Errorf("%d stops, %d suspensions: %q, want %q", tc.stops, tc.suspensions, got, tc.want)
		}
	}
}

// A lasting suspension that a new launch lifted is not reported, even
// while a later one of the new launch is in force.
func TestOnlyTheSuspensionInForceIsReported(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	p.lastingSuspension()
	p.now = p.now.Add(time.Minute)
	enrollAndRoute(t, p.fixture, p.reader, "pid-43-start", "resume", publishTestSocket, "default", p.now.UnixMilli(), 1)
	p.proveRoute()
	p.turnEndAsk(p.writer, "reader", "after-relaunch")
	p.requirePublish("a new launch lifts the lasting suspension", "available", "")
	// The route and state helpers run owner commands; the owner has not
	// seen the first launch's suspension yet.
	p.mutate(`UPDATE owner_alert SET shown_at_ms = NULL`)
	p.now = p.now.Add(time.Minute)
	second := p.lastingSuspension()
	if n := p.count(`SELECT COUNT(*) FROM owner_alert WHERE shown_at_ms IS NULL`); n != 2 {
		t.Fatalf("unshown alerts before the owner command: %d, want both launches' suspensions", n)
	}
	lines := p.owner(OwnerStatus{}, "").OwnerAlerts
	if len(lines) != 2 || !strings.Contains(lines[0], fmt.Sprintf("notice %d absorbed", second)) {
		t.Fatalf("owner line %q, want only notice %d's suspension", lines, second)
	}
}

// A hand join from a newer process starts a new launch but can leave the
// route naming the old one. The old launch's lasting suspension keeps its
// audit event and stops reaching the owner, on the line and the desktop.
func TestALastingSuspensionOfAnEarlierLaunchIsNotReported(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	p.lastingSuspension()
	p.now = p.now.Add(time.Minute)
	p.svc.LaunchProcess = testProcess
	p.svc.LaunchAlive = func(string, int64) bool { return true } // the old process too
	joined := decode[JoinResult](t, p.join(p.reader, Join{Channel: "checkout", Alias: "reader",
		Launch: joinLaunch("pid-43-join", p.now.UnixMilli())}, "hand-join"))
	if joined.Launch == nil || joined.Launch.Ref != "pid-43-join" {
		t.Fatalf("the hand join bound no new launch: %+v", joined.Launch)
	}
	if got := p.owner(OwnerStatus{}, "").OwnerAlerts; got != nil {
		t.Fatalf("an earlier launch's suspension reached the owner: %q", got)
	}
	p.mutate(`UPDATE owner_alert SET shown_at_ms = NULL`)
	var alerts []DesktopAlert
	p.update(func(ctx context.Context, tx store.Tx) (err error) {
		alerts, err = ClaimDesktopAlerts(ctx, tx, p.now, true)
		return err
	})
	if len(alerts) != 0 {
		t.Fatalf("an earlier launch's suspension was shown: %+v", alerts)
	}
	if events := p.suspensionEvents(); len(events) != 3 || !events[2].detail.Lasting || !events[2].alerted {
		t.Fatalf("the audit event was lost: %+v", events)
	}
}
