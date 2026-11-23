package domain

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// threadStop makes s's next request on a new thread to the alias a round
// stop, and returns the thread's first request.
func (f *fixture) threadStop(s session, to string) AskResult {
	f.t.Helper()
	parent := f.guardAsk(s, Ask{To: []string{to}, Body: "deep"}, "")
	executeGuardFixture(f.t, f, `UPDATE message SET level_ms = ? WHERE id = ?`, ms(f.now)+8*roundDurationMS, parent.ID)
	wantCode(f.t, f.ask(s, Ask{Re: parent.ID, To: []string{to}, Body: "follow up"}, ""), core.ExitRefused, core.CodeRoundLimit)
	return parent
}

type resetEvent struct {
	subject string
	actor   int64
	atMS    int64
	detail  map[string]any
}

func (f *fixture) resetEvents() []resetEvent {
	f.t.Helper()
	rows, err := f.db.Query(`SELECT subject, actor_id, at_ms, detail FROM event WHERE kind = 'loop_guard_reset' ORDER BY seq`)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []resetEvent
	for rows.Next() {
		var e resetEvent
		var raw string
		if err := rows.Scan(&e.subject, &e.actor, &e.atMS, &raw); err != nil {
			f.t.Fatal(err)
		}
		if err := json.Unmarshal([]byte(raw), &e.detail); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, e)
	}
	return out
}

func (f *fixture) guardStops() []GuardStop {
	f.t.Helper()
	return decode[StatusResult](f.t, f.owner(OwnerStatus{Channel: "checkout"}, "")).Channels[0].GuardStops
}

func TestEachRoundStopAddsOneOwnerAlert(t *testing.T) {
	f, a, _, _ := channel3(t)
	parent := f.guardAsk(a, Ask{To: []string{"tax"}, Body: "deep"}, "")
	executeGuardFixture(t, f, `UPDATE message SET level_ms = ? WHERE id = ?`, ms(f.now)+8*roundDurationMS, parent.ID)
	for i := 0; i < 10; i++ {
		wantCode(t, f.ask(a, Ask{Re: parent.ID, To: []string{"tax"}, Body: "follow up"}, ""), core.ExitRefused, core.CodeRoundLimit)
	}
	f.grantMember("form", 0)
	for i := 0; i < 10; i++ {
		wantCode(t, f.ask(a, Ask{New: true, To: []string{"tax"}, Body: "new"}, ""), core.ExitRefused, core.CodeRoundLimit)
	}
	for _, subject := range []string{fmt.Sprintf("exchange:%d", parent.Exchange), fmt.Sprintf("principal:%d", f.principalOf(a))} {
		if n := f.count(`SELECT COUNT(*) FROM owner_alert a JOIN event e ON e.seq = a.event_seq
			WHERE e.kind = ? AND e.subject = ? AND a.desktop_state = 'pending' AND a.desktop_at_ms IS NULL`, guardStopped, subject); n != 1 {
			t.Fatalf("%s: %d pending alerts, want 1", subject, n)
		}
	}
	if n := f.count(`SELECT COUNT(*) FROM owner_alert`); n != 2 {
		t.Fatalf("%d alerts for two stops", n)
	}
}

func TestEachGrantFormWritesOneResetEvent(t *testing.T) {
	f, a, _, _ := channel3(t)
	parent := f.threadStop(a, "tax")
	var owner int64
	if err := f.db.QueryRow(`SELECT id FROM principal WHERE kind = 'owner'`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	fence := f.count(`SELECT MAX(id) FROM message`)
	grant := BudgetGrant{Channel: "checkout", ExchangeID: parent.Exchange, Rounds: rounds(0)}
	wantOK(t, f.owner(grant, "thread"))
	wantOK(t, f.owner(grant, "thread"))
	events := f.resetEvents()
	want := map[string]any{"channel": "checkout", "rounds": float64(0), "reset_after_message_id": float64(fence), "was_stopped": true}
	if len(events) != 1 || events[0].subject != fmt.Sprintf("exchange:%d", parent.Exchange) || events[0].actor != owner ||
		events[0].atMS != ms(f.now) || !reflect.DeepEqual(events[0].detail, want) {
		t.Fatalf("thread grant and its replay: %+v, want one with %v", events, want)
	}
	resetAt := ms(f.now)
	f.now = f.now.Add(time.Minute)
	wantCode(t, f.ask(a, Ask{Re: parent.ID, To: []string{"tax"}, Body: "after"}, ""), core.ExitRefused, core.CodeRoundLimit)
	stops := f.guardStops()
	if len(stops) != 1 || stops[0].LastResetAtMS == nil || *stops[0].LastResetAtMS != resetAt {
		t.Fatalf("last reset of the thread stop: %+v", stops)
	}

	wantOK(t, f.owner(BudgetGrant{Channel: "checkout", Alias: "form", Rounds: rounds(0)}, "member"))
	wantOK(t, f.owner(BudgetGrant{Channel: "checkout", Alias: "form", Rounds: rounds(0)}, "member"))
	events = f.resetEvents()
	want = map[string]any{"channel": "checkout", "rounds": float64(0), "reset_after_message_id": float64(fence), "was_stopped": false}
	if len(events) != 2 || events[1].subject != fmt.Sprintf("principal:%d", f.principalOf(a)) || events[1].actor != owner ||
		!reflect.DeepEqual(events[1].detail, want) {
		t.Fatalf("member grant and its replay: %+v, want a second with %v", events, want)
	}
	memberReset := ms(f.now)
	f.now = f.now.Add(time.Minute)
	wantCode(t, f.ask(a, Ask{New: true, To: []string{"tax"}, Body: "new"}, ""), core.ExitRefused, core.CodeRoundLimit)
	stops = f.guardStops()
	if len(stops) != 2 || stops[1].Alias != "form" || stops[1].LastResetAtMS == nil || *stops[1].LastResetAtMS != memberReset {
		t.Fatalf("last reset of the member stop: %+v", stops)
	}
	wantCode(t, f.owner(BudgetGrant{Channel: "checkout", ExchangeID: 999, Rounds: rounds(8)}, "missing"), core.ExitRefused, core.CodeNotFound)
	if len(f.resetEvents()) != 2 {
		t.Fatal("a grant that took no effect wrote a reset event")
	}
}

func TestBindingGrantClosesADepartedMembersStop(t *testing.T) {
	f, a, _, _ := channel3(t)
	f.grantMember("form", 0)
	wantCode(t, f.ask(a, Ask{New: true, To: []string{"tax"}, Body: "stopped"}, ""), core.ExitRefused, core.CodeRoundLimit)
	binding := f.guardStops()[0].BindingID
	principal := f.principalOf(a)
	wantOK(t, f.run(a, Leave{}, "leave"))
	replacement := f.newSession("route")
	wantOK(t, f.join(replacement, Join{Channel: "checkout", Alias: "form"}, "join"))
	reused := func() string {
		var out string
		if err := f.db.QueryRow(`SELECT round_limit || '/' || guard_told || '/' || reset_after_message_id || '/' || any_level_ms
			FROM principal WHERE id = ?`, f.principalOf(replacement)).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := reused()

	wantCode(t, f.owner(BudgetGrant{BindingID: 9999, Rounds: rounds(5)}, "unknown"), core.ExitRefused, core.CodeNotFound)
	r := f.owner(BudgetGrant{BindingID: binding, Rounds: rounds(5)}, "by-binding")
	wantOK(t, r)
	if got := decode[BudgetGrantResult](t, r); got.Channel != "checkout" || got.Alias != "form" || got.BindingID != binding ||
		got.Rounds == nil || *got.Rounds != 5 {
		t.Fatalf("binding grant result: %+v", got)
	}
	wantOK(t, f.owner(BudgetGrant{BindingID: binding, Rounds: rounds(5)}, "by-binding"))
	if stops := f.guardStops(); len(stops) != 0 {
		t.Fatalf("the departed member's stop is still listed: %+v", stops)
	}
	events := f.resetEvents()
	if len(events) != 2 || events[1].subject != fmt.Sprintf("principal:%d", principal) || events[1].detail["was_stopped"] != true ||
		events[1].detail["rounds"] != float64(5) {
		t.Fatalf("binding grant reset events: %+v", events)
	}
	if after := reused(); after != before {
		t.Fatalf("the member that reused the alias changed: %s, was %s", after, before)
	}

	wantOK(t, f.join(a, Join{Channel: "checkout", Alias: "form-back"}, "return"))
	if got := f.guardAsk(a, Ask{New: true, To: []string{"tax"}, Body: "back"}, ""); got.Round != 1 {
		t.Fatalf("first request after the return: %+v", got)
	}
	if limit := f.count(`SELECT round_limit FROM principal WHERE id = ?`, principal); limit != 5 {
		t.Fatalf("returned member's limit is %d, want the granted 5", limit)
	}
}

func TestOwnerStatusDetailsAStop(t *testing.T) {
	f, a, b, _ := channel3(t)
	first := f.guardAsk(a, Ask{To: []string{"tax"}, Body: "one"}, "")
	second := f.guardAsk(b, Ask{Re: first.ID, To: []string{"form"}, Body: "two"}, "")
	third := f.guardAsk(a, Ask{Re: second.ID, To: []string{"tax", "a11y"}, Body: "three"}, "")
	executeGuardFixture(t, f, `UPDATE message SET thread_reset_after_message_id = ? WHERE id = ?`, first.ID, first.ID)
	executeGuardFixture(t, f, `UPDATE message SET level_ms = ? WHERE id = ?`, ms(f.now)+8*roundDurationMS, third.ID)
	wantCode(t, f.ask(b, Ask{Re: third.ID, To: []string{"form"}, Body: "four"}, ""), core.ExitRefused, core.CodeRoundLimit)
	executeGuardFixture(t, f, `UPDATE binding SET alias = 'form2' WHERE alias = 'form'`)

	raw := f.owner(OwnerStatus{Channel: "checkout"}, "")
	stops := decode[StatusResult](t, raw).Channels[0].GuardStops
	want := []StopRequest{
		{ID: second.ID, Sender: "tax", Recipients: []string{"form"}},
		{ID: third.ID, Sender: "form2", Recipients: []string{"a11y", "tax"}},
	}
	if len(stops) != 1 || stops[0].Sender != "tax" || !reflect.DeepEqual(stops[0].Recipients, []string{"form"}) ||
		stops[0].Round != 9 || !reflect.DeepEqual(stops[0].Requests, want) || stops[0].RequestCount != 2 || stops[0].LastResetAtMS != nil {
		t.Fatalf("thread stop detail: %+v", stops)
	}
	var fields struct {
		Channels []struct {
			GuardStops []map[string]json.RawMessage `json:"guard_stops"`
		} `json:"channels"`
	}
	if err := json.Unmarshal(raw.Data, &fields); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"sender", "recipients", "requests", "request_count"} {
		if fields.Channels[0].GuardStops[0][key] == nil {
			t.Errorf("guard_stops entry lacks %q: %s", key, raw.Data)
		}
	}
	if fields.Channels[0].GuardStops[0]["last_reset_at_ms"] != nil {
		t.Errorf("last_reset_at_ms without a reset event: %s", raw.Data)
	}
}

func TestOwnerStatusListsAMemberStopWithoutRequests(t *testing.T) {
	f, a, _, _ := channel3(t)
	f.grantMember("form", 0)
	wantCode(t, f.ask(a, Ask{New: true, To: []string{"tax", "a11y"}, Body: "stopped"}, ""), core.ExitRefused, core.CodeRoundLimit)
	stops := f.guardStops()
	if len(stops) != 1 || stops[0].Sender != "form" || !reflect.DeepEqual(stops[0].Recipients, []string{"a11y", "tax"}) ||
		stops[0].Requests != nil || stops[0].RequestCount != 0 || stops[0].LastResetAtMS == nil {
		t.Fatalf("member stop detail: %+v", stops)
	}
}

func TestOwnerStatusListsBothRootsOfAnOldExchange(t *testing.T) {
	f, a, _, _ := channel3(t)
	deep := f.guardAsk(a, Ask{To: []string{"tax"}, Body: "deep"}, "")
	sibling := f.guardAsk(a, Ask{To: []string{"a11y"}, Body: "shallow"}, "")
	executeGuardFixture(t, f, `UPDATE message SET level_ms = ? WHERE id = ?`, ms(f.now)+8*roundDurationMS, deep.ID)
	executeGuardFixture(t, f, `UPDATE message SET exchange_id = ? WHERE id = ?`, deep.Exchange, sibling.ID)
	wantCode(t, f.ask(a, Ask{Re: deep.ID, To: []string{"tax"}, Body: "follow up"}, ""), core.ExitRefused, core.CodeRoundLimit)
	stops := f.guardStops()
	if len(stops) != 1 || len(stops[0].Requests) != 2 || stops[0].Requests[0].ID != deep.ID || stops[0].Requests[1].ID != sibling.ID {
		t.Fatalf("old exchange with two roots: %+v", stops)
	}
}

func TestOwnerStatusCapsTheRequestList(t *testing.T) {
	f, a, _, _ := channel3(t)
	root := f.guardAsk(a, Ask{To: []string{"tax"}, Body: "root"}, "")
	ids := []int64{root.ID}
	for len(ids) < 53 {
		executeGuardFixture(t, f, `INSERT INTO message (channel_id, sender_id, kind, exchange_id, audience_mode, due_at_ms, body,
			created_at_ms, thread_root_id) SELECT channel_id, sender_id, kind, exchange_id, audience_mode, due_at_ms, body, created_at_ms - 1200000, id
			FROM message WHERE id = ?`, root.ID)
		ids = append(ids, int64(f.count(`SELECT MAX(id) FROM message`)))
		executeGuardFixture(t, f, `INSERT INTO recipient (message_id, principal_id, alias_at_commit, owes)
			SELECT ?, principal_id, alias_at_commit, owes FROM recipient WHERE message_id = ?`, ids[len(ids)-1], root.ID)
	}
	executeGuardFixture(t, f, `UPDATE message SET level_ms = ? WHERE id = ?`, ms(f.now)+8*roundDurationMS, root.ID)
	wantCode(t, f.ask(a, Ask{Re: root.ID, To: []string{"tax"}, Body: "stop"}, ""), core.ExitRefused, core.CodeRoundLimit)
	stops := f.guardStops()
	if len(stops) != 1 || stops[0].RequestCount != 53 || len(stops[0].Requests) != 50 {
		t.Fatalf("capped list: count %d, %d entries", stops[0].RequestCount, len(stops[0].Requests))
	}
	var got []int64
	for _, r := range stops[0].Requests {
		got = append(got, r.ID)
	}
	if want := append(append([]int64{}, ids[:2]...), ids[5:]...); !reflect.DeepEqual(got, want) {
		t.Fatalf("capped list keeps %v, want the first two and the last 48", got)
	}
}

func (f *fixture) claimDesktop(available bool) []DesktopAlert {
	f.t.Helper()
	tx, err := f.db.Begin()
	if err != nil {
		f.t.Fatal(err)
	}
	defer tx.Rollback()
	alerts, err := ClaimDesktopAlerts(context.Background(), tx, f.now, available)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		f.t.Fatal(err)
	}
	return alerts
}

func (f *fixture) inTx(apply func(store.Tx) error) {
	f.t.Helper()
	tx, err := f.db.Begin()
	if err != nil {
		f.t.Fatal(err)
	}
	defer tx.Rollback()
	if err := apply(tx); err != nil {
		f.t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) desktopStates() []string {
	f.t.Helper()
	rows, err := f.db.Query(`SELECT desktop_state || CASE WHEN desktop_at_ms IS NULL THEN '' ELSE '@' || desktop_at_ms END
		FROM owner_alert ORDER BY event_seq`)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func TestDesktopAlertIsSentOnceAndSpacedAMinuteApart(t *testing.T) {
	f, a, _, _ := channel3(t)
	parent := f.threadStop(a, "tax")
	start := ms(f.now)
	alerts := f.claimDesktop(true)
	want := DesktopAlert{Title: "crosshall: round limit in #checkout",
		Body: fmt.Sprintf("x%d stopped. @form's request to @tax would be round 9 of 8. Run: crosshall owner status", parent.Exchange)}
	if len(alerts) != 1 || alerts[0].Title != want.Title || alerts[0].Body != want.Body || alerts[0].Seq == 0 {
		t.Fatalf("claimed %+v, want %+v", alerts, want)
	}
	f.inTx(func(tx store.Tx) error { return FinishDesktopAlert(context.Background(), tx, alerts[0].Seq, true) })
	if again := f.claimDesktop(true); len(again) != 0 {
		t.Fatalf("a sent stop was claimed again: %+v", again)
	}
	f.now = f.now.Add(59 * time.Second)
	f.grantMember("form", 0)
	wantCode(t, f.ask(a, Ask{New: true, To: []string{"tax"}, Body: "new"}, ""), core.ExitRefused, core.CodeRoundLimit)
	if second := f.claimDesktop(true); len(second) != 0 {
		t.Fatalf("a second stop inside a minute was sent: %+v", second)
	}
	if got := f.desktopStates(); !reflect.DeepEqual(got, []string{fmt.Sprintf("sent@%d", start), "skipped"}) {
		t.Fatalf("states %v", got)
	}
}

func TestDesktopAlertSkipsOldAndClosedStops(t *testing.T) {
	f, a, _, c := channel3(t)
	f.threadStop(a, "tax")
	f.now = f.now.Add(10*time.Minute + time.Millisecond)
	if alerts := f.claimDesktop(true); len(alerts) != 0 {
		t.Fatalf("a stop older than ten minutes was sent: %+v", alerts)
	}
	granted := f.threadStop(c, "form")
	f.grantThread(granted.Exchange, 8)
	r1 := f.newSession("route")
	wantOK(t, f.join(r1, Join{Channel: "checkout", Alias: "r1"}, ""))
	wantOK(t, f.join(f.newSession("route"), Join{Channel: "checkout", Alias: "r2"}, ""))
	drained := f.threadStop(r1, "r2")
	executeGuardFixture(t, f, `UPDATE exchange SET episode_open = 0 WHERE id = ?`, drained.Exchange)
	wantCode(t, f.ask(r1, Ask{Re: drained.ID, To: []string{"r2"}, Body: "again"}, ""), core.ExitRefused, core.CodeRoundLimit)
	alerts := f.claimDesktop(true)
	if len(alerts) != 1 || !strings.HasPrefix(alerts[0].Body, fmt.Sprintf("x%d stopped. @r1's", drained.Exchange)) {
		t.Fatalf("claimed %+v, want only the stop recorded again", alerts)
	}
	if got := f.desktopStates(); !reflect.DeepEqual(got, []string{"skipped", "skipped", "skipped", "sending@" + fmt.Sprint(ms(f.now))}) {
		t.Fatalf("states %v", got)
	}
}

func TestDesktopAlertUnavailableFailedAndInterrupted(t *testing.T) {
	f, a, _, c := channel3(t)
	f.threadStop(a, "tax")
	if alerts := f.claimDesktop(false); len(alerts) != 0 {
		t.Fatalf("claimed %+v without a notification command", alerts)
	}
	f.now = f.now.Add(time.Minute)
	f.threadStop(c, "form")
	failed := f.claimDesktop(true)
	f.inTx(func(tx store.Tx) error { return FinishDesktopAlert(context.Background(), tx, failed[0].Seq, false) })
	f.now = f.now.Add(time.Minute)
	f.grantMember("form", 0)
	wantCode(t, f.ask(a, Ask{New: true, To: []string{"tax"}, Body: "new"}, ""), core.ExitRefused, core.CodeRoundLimit)
	interrupted := f.claimDesktop(true)
	f.inTx(func(tx store.Tx) error { return FailUnfinishedDesktopAlerts(context.Background(), tx) })
	f.now = f.now.Add(time.Minute)
	if again := f.claimDesktop(true); len(failed) != 1 || len(interrupted) != 1 || len(again) != 0 {
		t.Fatalf("claims %+v, %+v, then %+v", failed, interrupted, again)
	}
	want := []string{"unavailable", fmt.Sprintf("failed@%d", ms(f.now)-2*60_000), fmt.Sprintf("failed@%d", ms(f.now)-60_000)}
	if got := f.desktopStates(); !reflect.DeepEqual(got, want) {
		t.Fatalf("states %v, want %v", got, want)
	}
}

func TestDesktopAlertTextNamesNoMessageAndShortensRecipients(t *testing.T) {
	f, a, _, _ := channel3(t)
	to := []string{"tax", "a11y"}
	for _, alias := range []string{"r1", "r2", "r3"} {
		wantOK(t, f.join(f.newSession("route"), Join{Channel: "checkout", Alias: alias}, ""))
		to = append(to, alias)
	}
	f.grantMember("form", 0)
	wantCode(t, f.ask(a, Ask{New: true, To: to, Body: "SECRET-BODY"}, ""), core.ExitRefused, core.CodeRoundLimit)
	alerts := f.claimDesktop(true)
	want := "@form's new threads stopped. @form's request to @a11y, @r1, @r2 and 2 more would be round 1 of 0. Run: crosshall owner status"
	if len(alerts) != 1 || alerts[0].Body != want || strings.Contains(alerts[0].Title+alerts[0].Body, "SECRET") {
		t.Fatalf("claimed %+v, want body %q", alerts, want)
	}
}

func TestRecipientListNamesThreeAtMost(t *testing.T) {
	for _, tc := range []struct {
		in   []string
		want string
	}{
		{[]string{"b"}, "@b"},
		{[]string{"b", "c"}, "@b and @c"},
		{[]string{"b", "c", "d"}, "@b, @c and @d"},
		{[]string{"b", "c", "d", "e"}, "@b, @c, @d and 1 more"},
	} {
		if got := RecipientList(tc.in); got != tc.want {
			t.Errorf("RecipientList(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A stop recorded before migration 024 has no owner_alert row.
func TestStopFromBeforeTheUpgradeIsListedButNotAnnounced(t *testing.T) {
	f, a, _, _ := channel3(t)
	f.threadStop(a, "tax")
	executeGuardFixture(t, f, `DELETE FROM owner_alert`)
	if alerts := f.claimDesktop(true); len(alerts) != 0 {
		t.Fatalf("an earlier stop was sent: %+v", alerts)
	}
	r := f.owner(OwnerStatus{Channel: "checkout"}, "")
	if stops := decode[StatusResult](t, r).Channels[0].GuardStops; len(stops) != 1 || r.OwnerAlerts != nil {
		t.Fatalf("an earlier stop: listed %+v, line %q", stops, r.OwnerAlerts)
	}
}

func clockAt(atMS int64) string { return time.UnixMilli(atMS).Format("15:04") }

func TestOwnerLineShowsAnOpenStopOnceAndNeverToAMember(t *testing.T) {
	f, a, b, _ := channel3(t)
	parent := f.threadStop(a, "tax")
	if r := f.who(a); r.OwnerAlerts != nil {
		t.Fatalf("a member command carried the owner line: %q", r.OwnerAlerts)
	}
	if r := f.ask(b, Ask{To: []string{"a11y"}, Body: "unrelated"}, ""); r.OwnerAlerts != nil {
		t.Fatalf("a member request carried the owner line: %q", r.OwnerAlerts)
	}
	want := []string{
		fmt.Sprintf("crosshall: round limit reached in #checkout on x%d at %s. @form's request to @tax was refused: it would be round 9 of 8.",
			parent.Exchange, clockAt(ms(f.now))),
		fmt.Sprintf("next: crosshall owner status --channel checkout shows the thread. crosshall owner budget grant checkout/x%d --rounds 8 lets it continue.",
			parent.Exchange),
	}
	if got := f.owner(OwnerStatus{}, "").OwnerAlerts; !reflect.DeepEqual(got, want) {
		t.Fatalf("first owner command: %q, want %q", got, want)
	}
	if got := f.owner(OwnerStatus{}, "").OwnerAlerts; got != nil {
		t.Fatalf("the stop was shown twice: %q", got)
	}
}

func TestOwnerLineNamesALiveOrDepartedMember(t *testing.T) {
	for _, departed := range []bool{false, true} {
		t.Run(fmt.Sprintf("departed=%t", departed), func(t *testing.T) {
			f, a, _, _ := channel3(t)
			f.grantMember("form", 0)
			wantCode(t, f.ask(a, Ask{New: true, To: []string{"tax"}, Body: "stopped"}, ""), core.ExitRefused, core.CodeRoundLimit)
			target := "checkout/@form"
			if departed {
				target = fmt.Sprint(f.count(`SELECT id FROM binding WHERE alias = 'form'`))
				wantOK(t, f.run(a, Leave{}, "leave"))
			}
			want := []string{
				fmt.Sprintf("crosshall: round limit reached in #checkout for @form's new threads at %s. Its request to @tax was refused: it would be round 1 of 0.",
					clockAt(ms(f.now))),
				"next: crosshall owner status --channel checkout shows it. crosshall owner budget grant " + target + " --rounds 8 lets @form open threads again.",
			}
			if got := f.owner(OwnerStatus{}, "").OwnerAlerts; !reflect.DeepEqual(got, want) {
				t.Fatalf("owner line %q, want %q", got, want)
			}
		})
	}
}

func TestOwnerLineSkipsClosedStopsAndCountsPastThree(t *testing.T) {
	f, a, _, c := channel3(t)
	granted := f.threadStop(a, "tax")
	if r := f.owner(BudgetGrant{Channel: "checkout", ExchangeID: granted.Exchange, Rounds: rounds(8)}, "grant"); r.OwnerAlerts != nil {
		t.Fatalf("the grant that closed the stop printed it: %q", r.OwnerAlerts)
	}
	drained := f.threadStop(c, "form")
	executeGuardFixture(t, f, `UPDATE exchange SET episode_open = 0 WHERE id = ?`, drained.Exchange)
	if r := f.owner(OwnerStatus{}, ""); r.OwnerAlerts != nil {
		t.Fatalf("a drained stop was printed: %q", r.OwnerAlerts)
	}
	if n := f.count(`SELECT COUNT(*) FROM owner_alert WHERE shown_at_ms IS NULL`); n != 0 {
		t.Fatalf("%d closed stops are still waiting to be shown", n)
	}
	var first int64
	for i := 0; i < 4; i++ {
		sender, recipient := f.newSession("route"), f.newSession("route")
		wantOK(t, f.join(sender, Join{Channel: "checkout", Alias: fmt.Sprintf("s%d", i)}, ""))
		wantOK(t, f.join(recipient, Join{Channel: "checkout", Alias: fmt.Sprintf("r%d", i)}, ""))
		stop := f.threadStop(sender, fmt.Sprintf("r%d", i))
		if i == 0 {
			first = stop.Exchange
		}
	}
	got := f.owner(OwnerStatus{}, "").OwnerAlerts
	if len(got) != 7 || !strings.Contains(got[0], fmt.Sprintf(" on x%d ", first)) || got[6] != "and 1 more: crosshall owner status" {
		t.Fatalf("four stops: %q", got)
	}
	if again := f.owner(OwnerStatus{}, "").OwnerAlerts; again != nil {
		t.Fatalf("the stops were printed again: %q", again)
	}
}

func TestNoHookOutputMentionsAStop(t *testing.T) {
	f, writer := adapterChannel(t)
	s := f.newSession("claude")
	t0 := f.now.UnixMilli()
	f.hook(s, start("pid-1", "startup", t0))
	f.grantMember("claude1", 0)
	wantCode(t, f.ask(s, Ask{New: true, To: []string{"writer"}, Body: "stopped"}, ""), core.ExitRefused, core.CodeRoundLimit)
	wantOK(t, f.ask(writer, Ask{To: []string{"claude1"}, Body: "hello"}, ""))
	for _, cmd := range []Command{prompt("pid-1", "t1", t0+1_000), stop("pid-1", "t1", t0+2_000)} {
		r := f.hookResult(s, cmd)
		wantOK(t, r)
		if text := strings.ToLower(string(r.Data)); strings.Contains(text, "round limit") || strings.Contains(text, "stop is recorded") ||
			strings.Contains(text, "budget grant") || r.OwnerAlerts != nil {
			t.Fatalf("%s hook output mentions the stop: %s", cmd.Verb(), r.Data)
		}
	}
}

// The dispatcher looks for pending stops in a read transaction each second
// and opens a write transaction only when one is there.
func TestPendingDesktopAlertsIsReadOnly(t *testing.T) {
	f, a, _, _ := channel3(t)
	pending := func() bool {
		t.Helper()
		var got bool
		f.inTx(func(tx store.Tx) error {
			var err error
			got, err = PendingDesktopAlerts(context.Background(), tx)
			return err
		})
		return got
	}
	if pending() {
		t.Fatal("pending without a stop")
	}
	f.threadStop(a, "tax")
	if !pending() {
		t.Fatal("a new stop is not pending")
	}
	f.claimDesktop(true)
	if pending() {
		t.Fatal("a claimed stop is still pending")
	}
}

// The owner line is an extra: an error while building it neither fails the
// owner's command nor rolls back its effect.
func TestOwnerLineFailureLeavesTheOwnerCommand(t *testing.T) {
	f := newFixture(t)
	if _, err := f.db.Exec(`INSERT INTO event (kind, subject, detail, at_ms) VALUES (?, 'principal:1', 'not json', 1)`, guardStopped); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO owner_alert (event_seq) VALUES (last_insert_rowid())`); err != nil {
		t.Fatal(err)
	}
	r := f.owner(CreateChannel{Name: "checkout"}, "")
	wantOK(t, r)
	if len(r.OwnerAlerts) != 0 {
		t.Fatalf("owner line from a broken stop: %q", r.OwnerAlerts)
	}
	if n := f.count(`SELECT COUNT(*) FROM channel WHERE name = 'checkout'`); n != 1 {
		t.Fatalf("the owner's command was rolled back: %d channels", n)
	}
}

// A line that fails after the stops were marked shown leaves them unshown:
// its writes are undone with it, and the owner's command still commits.
func TestOwnerLineFailureUndoesOnlyItsOwnWrites(t *testing.T) {
	f := newFixture(t)
	wantOK(t, f.owner(CreateChannel{Name: "checkout"}, ""))
	m := ledgerMember(t, f, "checkout", "form")
	principal := ledgerCaller(t, f, m)
	if _, err := f.db.Exec(`UPDATE principal SET guard_told = 1 WHERE id = ?`, principal); err != nil {
		t.Fatal(err)
	}
	// The stop names a channel the member is not in, so the line cannot be built.
	if _, err := f.db.Exec(`INSERT INTO event (kind, subject, detail, at_ms) VALUES (?, ?, '{"channel":"nowhere","sender":"form","recipients":["tax"],"round":9,"round_limit":8}', 1)`,
		guardStopped, fmt.Sprintf("principal:%d", principal)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.Exec(`INSERT INTO owner_alert (event_seq) VALUES (last_insert_rowid())`); err != nil {
		t.Fatal(err)
	}
	r := f.owner(CreateChannel{Name: "billing"}, "")
	wantOK(t, r)
	if len(r.OwnerAlerts) != 0 {
		t.Fatalf("owner line from a broken stop: %q", r.OwnerAlerts)
	}
	if n := f.count(`SELECT COUNT(*) FROM channel WHERE name = 'billing'`); n != 1 {
		t.Fatalf("the owner's command was rolled back: %d channels", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM owner_alert WHERE shown_at_ms IS NOT NULL`); n != 0 {
		t.Fatalf("a stop whose line failed was marked shown: %d", n)
	}
}
