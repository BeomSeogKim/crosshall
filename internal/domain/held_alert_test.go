package domain

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

// heldNotice submits a notice to the reader that no receipt follows.
func (p *publishFixture) heldNotice() int64 {
	p.t.Helper()
	ask := p.turnEndAsk(p.writer, "reader", fmt.Sprintf("held-%d", p.now.UnixNano()))
	id := p.reserve()
	if id == 0 || !slices.Equal(p.begin(id), []int64{ask}) {
		p.t.Fatal("reservation did not start")
	}
	p.resolve(id, PublishSubmitted)
	return id
}

func (p *publishFixture) alertLongHolds() {
	p.t.Helper()
	p.update(func(ctx context.Context, tx store.Tx) error { return AlertLongHolds(ctx, tx, p.now) })
}

// A notice without a receipt for fifteen minutes reaches the owner once,
// on the next owner command and on the desktop, with the command that
// releases it.
func TestALongHeldNoticeReachesTheOwnerOnce(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	p.finishPreviousTurn()
	since := p.now
	notice := p.heldNotice()
	p.now = since.Add(publishHeldAlertAfter - time.Second)
	p.alertLongHolds()
	if got := p.owner(OwnerStatus{}, "").OwnerAlerts; got != nil {
		t.Fatalf("a notice held under fifteen minutes alerted: %q", got)
	}
	p.now = since.Add(publishHeldAlertAfter)
	p.alertLongHolds()
	p.alertLongHolds()
	if n := p.count(`SELECT COUNT(*) FROM event WHERE kind = ?`, publishHeldEvent); n != 1 {
		t.Fatalf("%d held events, want 1", n)
	}
	if r := p.run(p.reader, Status{}, ""); r.OwnerAlerts != nil {
		t.Fatalf("a member command carried the owner line: %q", r.OwnerAlerts)
	}
	want := []string{
		fmt.Sprintf("crosshall: @reader in #checkout gets no notices: notice %d has had no receipt since %s.", notice, clockAt(since.UnixMilli())),
		fmt.Sprintf("next: crosshall owner publish resume %d releases it.", p.binding.ID),
	}
	if got := p.owner(OwnerStatus{}, "").OwnerAlerts; !reflect.DeepEqual(got, want) {
		t.Fatalf("first owner command: %q, want %q", got, want)
	}
	if got := p.owner(OwnerStatus{}, "").OwnerAlerts; got != nil {
		t.Fatalf("the held notice was shown twice: %q", got)
	}
	var alerts []DesktopAlert
	p.update(func(ctx context.Context, tx store.Tx) (err error) {
		alerts, err = ClaimDesktopAlerts(ctx, tx, p.now, true)
		return err
	})
	wantBody := fmt.Sprintf("@reader gets no notices: notice %d has had no receipt since %s. Run: crosshall owner publish resume %d",
		notice, clockAt(since.UnixMilli()), p.binding.ID)
	if len(alerts) != 1 || alerts[0].Title != "crosshall: notices held in #checkout" || alerts[0].Body != wantBody {
		t.Fatalf("desktop alerts %+v", alerts)
	}
}

// The owner's resume releases the held notice: the lane frees without an
// observation, the member's requests stay unread, and the alert closes.
func TestResumeReleasesAHeldNotice(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	p.finishPreviousTurn()
	notice := p.heldNotice()
	p.now = p.now.Add(publishHeldAlertAfter)
	p.alertLongHolds()
	if got := p.publishState(); got.State != "held" {
		t.Fatalf("before the resume: %+v", got)
	}
	r := decode[PublishSwitchResult](t, p.owner(OwnerPublish{BindingID: p.binding.ID, Switch: "resume"}, "resume"))
	if r.ReleasedNotice != notice {
		t.Fatalf("resume released %d, want %d", r.ReleasedNotice, notice)
	}
	if got := p.publishState(); got.State == "held" {
		t.Fatalf("after the resume: %+v", got)
	}
	p.observation(notice, "")
	if n := p.count(`SELECT COUNT(*) FROM event WHERE kind = 'publish_attempt_settled'
		AND json_extract(detail, '$.reason') = 'owner resume'`); n != 1 {
		t.Fatalf("%d owner-resume settlements", n)
	}
	if n := p.count(`SELECT COUNT(*) FROM recipient WHERE principal_id = ? AND picked_up_at_ms IS NOT NULL`, p.principalOf(p.reader)); n != 0 {
		t.Fatal("the release picked up a request")
	}
	var alerts []DesktopAlert
	p.update(func(ctx context.Context, tx store.Tx) (err error) {
		alerts, err = ClaimDesktopAlerts(ctx, tx, p.now, true)
		return err
	})
	if len(alerts) != 0 || p.count(`SELECT COUNT(*) FROM owner_alert WHERE desktop_state = 'skipped'`) != 1 {
		t.Fatalf("a released notice was shown: %+v", alerts)
	}
	again := decode[PublishSwitchResult](t, p.owner(OwnerPublish{BindingID: p.binding.ID, Switch: "resume"}, "resume-again"))
	if again.ReleasedNotice != 0 {
		t.Fatalf("a second resume released %d", again.ReleasedNotice)
	}
}

// A notice the transport has not answered is not released: it may still
// be on its way.
func TestResumeLeavesANoticeBeingSubmitted(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	p.finishPreviousTurn()
	ask := p.turnEndAsk(p.writer, "reader", "in-flight")
	id := p.reserve()
	if id == 0 || !slices.Equal(p.begin(id), []int64{ask}) {
		t.Fatal("reservation did not start")
	}
	r := decode[PublishSwitchResult](t, p.owner(OwnerPublish{BindingID: p.binding.ID, Switch: "resume"}, "resume"))
	if r.ReleasedNotice != 0 {
		t.Fatalf("resume released notice %d while it was submitted", r.ReleasedNotice)
	}
	p.state(id, "submitting")
}

// The held-notice query runs every publisher tick inside the dispatcher's
// write transaction: it must search the open-lane index and drive its
// dedup from owner_alert, never scan publish_attempt or event, which are
// not pruned.
func TestLongHeldNoticesQueryStaysASearch(t *testing.T) {
	p := newPublishFixture(t)
	rows, err := p.db.Query("EXPLAIN QUERY PLAN "+longHeldNotices, 0, publishHeldEvent)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	joined := strings.Join(plan, " | ")
	for _, step := range plan {
		if strings.HasPrefix(step, "SCAN a") || strings.HasPrefix(step, "SCAN e") {
			t.Fatalf("plan scans a growing table: %s", joined)
		}
	}
	for _, want := range []string{"SEARCH a USING INDEX publish_attempt_binding", "SEARCH e USING INTEGER PRIMARY KEY"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("plan lacks %q: %s", want, joined)
		}
	}
}

// A notice the transport has not answered yet is not alerted: a resume
// would not release it.
func TestANoticeStillBeingSubmittedIsNotAlerted(t *testing.T) {
	p := newPublishFixture(t)
	p.proveRoute()
	p.finishPreviousTurn()
	ask := p.turnEndAsk(p.writer, "reader", "in-flight")
	id := p.reserve()
	if id == 0 || !slices.Equal(p.begin(id), []int64{ask}) {
		t.Fatal("reservation did not start")
	}
	p.now = p.now.Add(publishHeldAlertAfter)
	p.alertLongHolds()
	if n := p.count(`SELECT COUNT(*) FROM event WHERE kind = ?`, publishHeldEvent); n != 0 {
		t.Fatalf("%d held events for a notice still being submitted", n)
	}
}
