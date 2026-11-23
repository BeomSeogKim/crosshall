package domain

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
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
