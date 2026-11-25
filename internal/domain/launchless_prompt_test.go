package domain

import (
	"encoding/json"
	"testing"
)

// A member that joined by hand, whose session start never reached the hall,
// has no launch. Its first prompt opens the session launch the start would
// have, so the prompt shows the pending line and the turn is tracked.
func TestAHandJoinedSessionsFirstPromptOpensItsLaunch(t *testing.T) {
	f, writer := adapterChannel(t)
	s := f.newSession("claude")
	wantOK(t, f.join(s, Join{Channel: "checkout", Alias: "claude1"}, ""))
	ids := f.noteTo(writer, "claude1", 1)
	t0 := f.now.UnixMilli()
	if got := f.hook(s, prompt(SessionLaunch, "t1", t0+1_000)); got.PendingLine != pendingLine(ids...) {
		t.Fatalf("first prompt %+v", got)
	}
	var raw string
	if err := f.db.QueryRowContext(f.t.Context(), `SELECT profile FROM adapter_launch WHERE launch_ref = ?`, SessionLaunch).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var p struct{ Mode string }
	if err := json.Unmarshal([]byte(raw), &p); err != nil || p.Mode != testProfile.Mode {
		t.Fatalf("launch profile %s, want the prompt's mode %q", raw, testProfile.Mode)
	}
	if n := f.count(`SELECT COUNT(*) FROM turn`); n != 1 {
		t.Fatalf("%d turns, want the prompt's", n)
	}
}

// A vendor that can run the session in a shared server reports that
// server's mode from its hooks, so a prompt that names no server opens the
// launch with its mode unverified.
func TestAHandJoinedSharedServerSessionOpensItsLaunchUnverified(t *testing.T) {
	f, writer := adapterChannel(t)
	f.policy("codex", true, 1)
	s := f.newSession("codex")
	wantOK(t, f.join(s, Join{Channel: "checkout", Alias: "codex1"}, ""))
	ids := f.noteTo(writer, "codex1", 1)
	e := prompt(SessionLaunch, "t1", f.now.UnixMilli()+1_000)
	e.Profile = codexProfile("in_process")
	if got := f.hook(s, e); got.PendingLine != pendingLine(ids...) {
		t.Fatalf("first prompt %+v", got)
	}
	var raw string
	if err := f.db.QueryRowContext(t.Context(), `SELECT profile FROM adapter_launch WHERE launch_ref = ?`, SessionLaunch).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var p struct{ Mode string }
	if err := json.Unmarshal([]byte(raw), &p); err != nil || p.Mode != "unverified" {
		t.Fatalf("launch profile %s, want an unverified mode", raw)
	}
}
