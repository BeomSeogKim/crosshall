package domain

import (
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
)

func TestSessionMoveFencesWaitThroughRoundTrip(t *testing.T) {
	f, mover, peer, _ := channel3(t)
	wait := func(cmd Wait) core.Result {
		return f.exec(f.caller(mover), Request{Command: cmd})
	}
	old := decode[WaitSnapshot](t, wait(Wait{Any: true})).Cursor
	if old.Generation != 1 {
		t.Fatalf("the initial membership has generation %d", old.Generation)
	}
	move := Join{Channel: "inventory", Move: true, Alias: "form"}
	wantOK(t, f.join(mover, move, "move-out"))
	other := f.newSession("claude")
	wantOK(t, f.join(other, Join{Channel: "inventory", Alias: "tax"}, "join-peer"))
	ask := decode[AskResult](t, f.ask(other, Ask{To: []string{"form"}, Body: "B only"}, "b-work"))
	stale := wait(Wait{Any: true, Cursor: &old})
	wantCode(t, stale, core.ExitRefused, core.CodeDeparted)
	if data := decode[struct{ Pending []int64 }](t, stale); len(data.Pending) != 0 {
		t.Fatalf("A wait learned B's pending ID %d: %+v", ask.ID, data)
	}
	wantOK(t, f.join(mover, Join{Channel: "checkout", Move: true}, "move-back"))
	before := decode[AskResult](t, f.ask(peer, Ask{To: []string{"form"}, Body: "A again"}, "a-work"))
	if before.Recipients[0].Next == WaitRunning {
		t.Fatalf("the new A membership reused the old wait: %+v", before.Recipients[0])
	}
	wantCode(t, wait(Wait{Any: true, Cursor: &old}), core.ExitRefused, core.CodeDeparted)
	now := decode[WaitSnapshot](t, wait(Wait{Any: true})).Cursor
	if now.BindingID != old.BindingID || now.Generation != 3 {
		t.Fatalf("A→B→A cursor %+v, originally %+v", now, old)
	}
	if replay := decode[JoinResult](t, f.join(mover, move, "move-out")); replay.Channel != "inventory" {
		t.Fatalf("expected the original move result, got %+v", replay)
	}
	if who := decode[WhoResult](t, f.who(mover)); who.Channel != "checkout" {
		t.Fatalf("replaying a move left the current channel: %+v", who)
	}
	wantOK(t, f.exec(f.caller(mover), Request{Command: Leave{}, OpKey: "leave-a"}))
	wantOK(t, f.join(mover, Join{Channel: "checkout"}, "rejoin-a"))
	wantOK(t, wait(Wait{Any: true, Cursor: &now}))
}

func TestSessionMoveCanLeaveAnArchivedSource(t *testing.T) {
	f, mover, _, _ := channel3(t)
	if _, err := f.db.Exec(`UPDATE channel SET state = 'archived' WHERE name = 'checkout'`); err != nil {
		t.Fatal(err)
	}
	wantCode(t, f.join(mover, Join{Channel: "inventory"}, "plain"), core.ExitRefused, core.CodeOneChannel)
	moved := decode[JoinResult](t, f.join(mover, Join{Channel: "inventory", Move: true}, "exit-archive"))
	if moved.Channel != "inventory" || moved.Move == nil || moved.Move.Channel != "checkout" {
		t.Fatalf("moving from an archive: %+v", moved)
	}
	wantCode(t, f.join(mover, Join{Channel: "checkout", Move: true}, "into-archive"), core.ExitRefused, core.CodeArchived)
	if who := decode[WhoResult](t, f.who(mover)); who.Channel != "inventory" {
		t.Fatalf("a refused move changed the current channel: %+v", who)
	}
}

func TestSessionMoveLeavesThePrincipalGuardStopInItsOriginChannel(t *testing.T) {
	f, mover, _, _ := channel3(t)
	principal := f.principalOf(mover)
	level := f.now.UnixMilli() + 9*roundDurationMS
	if _, err := f.db.Exec(`UPDATE principal SET any_level_ms = ? WHERE id = ?`, level, principal); err != nil {
		t.Fatal(err)
	}
	wantCode(t, f.ask(mover, Ask{To: []string{"tax"}, Body: "guard stop"}, ""), core.ExitRefused, core.CodeRoundLimit)
	wantOK(t, f.join(mover, Join{Channel: "inventory", Move: true}, "move"))
	status := decode[StatusResult](t, f.owner(Status{}, ""))
	for _, channel := range status.Channels {
		want := 0
		if channel.Name == "checkout" {
			want = 1
		}
		if len(channel.GuardStops) != want {
			t.Fatalf("channel %s has guard stops %+v, want %d", channel.Name, channel.GuardStops, want)
		}
	}
	var got int64
	if err := f.db.QueryRow(`SELECT any_level_ms FROM principal WHERE id = ?`, principal).Scan(&got); err != nil || got != level {
		t.Fatalf("move changed principal round level from %d to %d: %v", level, got, err)
	}
}
