package domain

import (
	"slices"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/publish"
)

// A Codex member whose session ended says so to a sender instead of
// "notices off": nothing switched its notices off.
func TestASenderReadsThatTheSessionEnded(t *testing.T) {
	f, writer := adapterChannel(t)
	f.policy("codex", true, 1)
	reader, _ := codexMember(t, f, "reader")
	f.hook(reader, codexEnded(publish.ModeDaemon, f.now.UnixMilli()+1))
	r := decode[AskResult](t, f.ask(writer, Ask{To: []string{"reader"}, New: true, Body: "after the end"}, "after-end"))
	if len(r.Recipients) != 1 || r.Recipients[0].Why != WhyEnded || !r.Recipients[0].NeedsManualWake() {
		t.Fatalf("recipients %+v, want why %q", r.Recipients, WhyEnded)
	}
}

// Members whose session ended over 12 hours ago, with nothing seen from
// them since, are listed for the owner; depart --ended departs those that
// owe nothing and keeps the others.
func TestOwnerDepartsEndedMembersThatOweNothing(t *testing.T) {
	f, writer := adapterChannel(t)
	f.policy("codex", true, 1)
	idle, idleBinding := codexMember(t, f, "idle")
	owing, owingBinding := codexMember(t, f, "owing")
	back, _ := codexMember(t, f, "back")
	_, liveBinding := codexMember(t, f, "live")
	ask := f.turnEndAsk(writer, "owing", "owed")
	endedAt := f.now.UnixMilli() + 1
	for _, s := range []session{idle, owing, back} {
		f.hook(s, codexEnded(publish.ModeDaemon, endedAt))
	}

	f.now = f.now.Add(endedDepartAfter - time.Minute)
	if got := decode[OwnerWhoResult](t, f.owner(OwnerWho{}, "")).Ended; len(got) != 0 {
		t.Fatalf("listed before 12 hours: %+v", got)
	}
	f.now = f.now.Add(2 * time.Minute)
	wantOK(t, f.exec(f.caller(back), Request{Command: Status{}}))
	// The writer joined by hand (no launch); a command keeps it off the list.
	wantOK(t, f.exec(f.caller(writer), Request{Command: Status{}}))
	f.now = f.now.Add(time.Minute)

	ended := decode[OwnerWhoResult](t, f.owner(OwnerWho{}, "")).Ended
	if len(ended) != 2 || ended[0].Alias != "idle" || ended[0].BindingID != idleBinding || len(ended[0].OpenRequests) != 0 ||
		ended[1].Alias != "owing" || ended[1].BindingID != owingBinding || !slices.Equal(ended[1].OpenRequests, []int64{ask}) {
		t.Fatalf("ended members %+v", ended)
	}

	r := decode[OwnerDepartEndedResult](t, f.owner(OwnerDepartEnded{}, "depart-ended"))
	if len(r.Departed) != 1 || r.Departed[0].BindingID != idleBinding || len(r.Owing) != 1 || r.Owing[0].BindingID != owingBinding {
		t.Fatalf("depart --ended: %+v", r)
	}
	states := map[int64]string{}
	rows, err := f.db.Query(`SELECT id, state FROM binding`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		var state string
		if err := rows.Scan(&id, &state); err != nil {
			t.Fatal(err)
		}
		states[id] = state
	}
	rows.Close()
	if states[idleBinding] != "departed" || states[owingBinding] != "live" || states[liveBinding] != "live" {
		t.Fatalf("binding states %v", states)
	}
	if n := f.count(`SELECT COUNT(*) FROM event WHERE kind = 'binding_departed' AND subject = ?`, "binding:"+itoa(idleBinding)); n != 1 {
		t.Fatalf("%d departure events", n)
	}
	again := decode[OwnerDepartEndedResult](t, f.owner(OwnerDepartEnded{}, "depart-ended"))
	if len(again.Departed) != 1 || again.Departed[0].BindingID != idleBinding {
		t.Fatalf("the replay differs: %+v", again)
	}
}

// Vega's #334 cases: a member that awaits a reply, a member that moved and
// still owes one in its prior channel, a command in the same millisecond as
// the session's end, and a session ended exactly 12 hours ago are all kept.
func TestDepartEndedKeepsEveryMemberWithOpenWorkOrRecentActivity(t *testing.T) {
	f, writer := adapterChannel(t)
	f.policy("codex", true, 1)
	wantOK(t, f.owner(CreateChannel{Name: "other"}, "other"))
	asker, _ := codexMember(t, f, "asker")
	mover, _ := codexMember(t, f, "mover")
	sameMS, _ := codexMember(t, f, "samems")
	idle, idleBinding := codexMember(t, f, "idle")
	r := decode[AskResult](t, f.ask(asker, Ask{To: []string{"writer"}, New: true, Body: "an answer, please"}, "asked"))
	owed := f.turnEndAsk(writer, "mover", "owed-before-move")
	wantOK(t, f.join(mover, Join{Channel: "other", Alias: "mover", Move: true}, "move"))
	endedAt := f.now.UnixMilli() + 1
	for _, s := range []session{asker, mover, sameMS, idle} {
		f.hook(s, codexEnded(publish.ModeDaemon, endedAt))
	}
	wantOK(t, f.exec(f.caller(sameMS), Request{Command: Status{}}))

	f.now = f.now.Add(endedDepartAfter)
	wantOK(t, f.exec(f.caller(writer), Request{Command: Status{}}))
	if got := decode[OwnerWhoResult](t, f.owner(OwnerWho{}, "")).Ended; len(got) != 0 {
		t.Fatalf("listed at exactly 12 hours: %+v", got)
	}
	f.now = f.now.Add(time.Millisecond)
	byAlias := map[string]EndedMember{}
	for _, m := range decode[OwnerWhoResult](t, f.owner(OwnerWho{}, "")).Ended {
		byAlias[m.Alias] = m
	}
	if _, listed := byAlias["samems"]; listed || len(byAlias) != 3 ||
		!slices.Equal(byAlias["asker"].OpenRequests, []int64{r.ID}) ||
		!slices.Equal(byAlias["mover"].OpenRequests, []int64{owed}) || len(byAlias["idle"].OpenRequests) != 0 {
		t.Fatalf("ended members %+v", byAlias)
	}
	out := decode[OwnerDepartEndedResult](t, f.owner(OwnerDepartEnded{}, "depart-ended"))
	if len(out.Departed) != 1 || out.Departed[0].BindingID != idleBinding || len(out.Owing) != 2 {
		t.Fatalf("depart --ended: %+v", out)
	}
}

// A member whose session start never reached the hall has no launch and so
// no route a session end could close (the old @agy in #dev). Once nothing,
// not even a command, was seen from it for 12 hours, it is listed and
// depart --ended departs it if it owes nothing; open requests keep it.
func TestDepartEndedCoversAMemberWithoutASessionStart(t *testing.T) {
	f, writer := adapterChannel(t)
	silent := f.newSession("antigravity")
	wantOK(t, f.join(silent, Join{Channel: "checkout", Alias: "silent"}, ""))
	owing := f.newSession("antigravity")
	wantOK(t, f.join(owing, Join{Channel: "checkout", Alias: "owing"}, ""))
	ask := f.turnEndAsk(writer, "owing", "owed")
	recent := f.newSession("antigravity")
	wantOK(t, f.join(recent, Join{Channel: "checkout", Alias: "recent"}, ""))
	joined := f.now.UnixMilli()

	f.now = f.now.Add(endedDepartAfter)
	wantOK(t, f.exec(f.caller(writer), Request{Command: Status{}}))
	wantOK(t, f.exec(f.caller(recent), Request{Command: Status{}}))
	if got := decode[OwnerWhoResult](t, f.owner(OwnerWho{}, "")).Ended; len(got) != 0 {
		t.Fatalf("listed at exactly 12 hours: %+v", got)
	}
	f.now = f.now.Add(time.Millisecond)
	ended := decode[OwnerWhoResult](t, f.owner(OwnerWho{}, "")).Ended
	if len(ended) != 2 || ended[0].Alias != "owing" || !ended[0].NoStart || !slices.Equal(ended[0].OpenRequests, []int64{ask}) ||
		ended[1].Alias != "silent" || !ended[1].NoStart || ended[1].EndedAtMS != joined || len(ended[1].OpenRequests) != 0 {
		t.Fatalf("ended members %+v", ended)
	}
	r := decode[OwnerDepartEndedResult](t, f.owner(OwnerDepartEnded{}, "depart-ended"))
	if len(r.Departed) != 1 || r.Departed[0].Alias != "silent" || len(r.Owing) != 1 || r.Owing[0].Alias != "owing" {
		t.Fatalf("depart --ended: %+v", r)
	}
	if n := f.count(`SELECT COUNT(*) FROM binding WHERE state = 'live'`); n != 3 {
		t.Fatalf("%d live bindings, want writer, owing and recent", n)
	}
}
