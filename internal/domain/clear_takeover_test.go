package domain

import (
	"strings"
	"testing"
)

// clearFixture joins a hooked Claude member in process pid-7 and gives it
// one ask it has read (owed) and one it has not (unread).
func clearFixture(t *testing.T, alive bool) (*fixture, session, session, int64, int64) {
	t.Helper()
	f, writer := adapterChannel(t)
	f.svc.LaunchProcess = testProcess
	f.svc.LaunchAlive = func(string, int64) bool { return alive }
	old := f.newSession("claude")
	f.hook(old, start("pid-7-a", "startup", f.now.UnixMilli()))
	owed := decode[AskResult](t, f.ask(writer, Ask{To: []string{"claude1"}, Body: "owed"}, "k1")).ID
	f.pickUp(old, owed)
	unread := decode[AskResult](t, f.ask(writer, Ask{To: []string{"claude1"}, Body: "unread"}, "k2")).ID
	return f, writer, old, owed, unread
}

func (f *fixture) keyState(s session) string {
	f.t.Helper()
	var state string
	if err := f.db.QueryRow(`SELECT state FROM principal_key WHERE vendor = ? AND key = ?`, s.vendor, s.key).Scan(&state); err != nil {
		f.t.Fatal(err)
	}
	return state
}

func wantTakenOver(t *testing.T, f *fixture, writer, old, cleared session, owed, unread int64, got string) {
	t.Helper()
	if f.keyState(old) != "retired" {
		t.Fatal("the cleared conversation's key is still live")
	}
	if n := f.count(`SELECT COUNT(*) FROM binding WHERE alias = 'claude1' AND state = 'live'`); n != 1 {
		t.Fatalf("live claude1 bindings = %d, want the one member", n)
	}
	if n := f.count(`SELECT COUNT(*) FROM binding`); n != 2 {
		t.Fatalf("bindings = %d, want the writer and the one member", n)
	}
	inbox := decode[InboxResult](t, f.run(cleared, Inbox{}, ""))
	if len(inbox.Unread) != 1 || inbox.Unread[0].ID != unread || len(inbox.Owed) != 1 || inbox.Owed[0].ID != owed {
		t.Fatalf("the cleared session's inbox: %+v, want unread %d and owed %d", inbox, unread, owed)
	}
	if !strings.Contains(got, ResetHint("claude1", "checkout")) || !strings.Contains(got, "1 owed") {
		t.Fatalf("reset cue %q does not say who it is and what is pending", got)
	}
	if m := f.memberRow(writer, "claude1"); m.Alias == "" {
		t.Fatal("the asker sees the member departed")
	}
}

func TestClearAfterTheEndKeepsTheMember(t *testing.T) {
	f, writer, old, owed, unread := clearFixture(t, true)
	f.hook(old, ended("pid-7-a", f.now.UnixMilli()+10))
	cleared := f.newSession("claude")
	r := f.hook(cleared, start("pid-7-b", "clear", f.now.UnixMilli()+20))
	if r.Enrollment != "restored" || r.Alias != "claude1" {
		t.Fatalf("clear start: %+v, want claude1 restored", r)
	}
	wantTakenOver(t, f, writer, old, cleared, owed, unread, r.Cue+"\n"+r.PendingLine)
}

func TestClearBeforeTheEndKeepsTheMember(t *testing.T) {
	f, writer, old, owed, unread := clearFixture(t, true)
	cleared := f.newSession("claude")
	r := f.hook(cleared, start("pid-7-b", "clear", f.now.UnixMilli()+20))
	if r.Enrollment != "restored" || r.Alias != "claude1" {
		t.Fatalf("clear start: %+v, want claude1 restored", r)
	}
	if n := f.count(`SELECT COUNT(*) FROM adapter_launch WHERE launch_ref = 'pid-7-a' AND ended_hook_at_ms IS NULL`); n != 0 {
		t.Fatal("the replaced launch is still open")
	}
	wantTakenOver(t, f, writer, old, cleared, owed, unread, r.Cue+"\n"+r.PendingLine)
}

func TestOnlyAClearInTheSameRunningProcessTakesOver(t *testing.T) {
	for _, c := range []struct {
		name, launch, source string
		alive, left          bool
	}{
		{"another process", "pid-8-b", "clear", true, false},
		{"a startup", "pid-7-b", "startup", true, false},
		{"a reused pid", "pid-7-b", "clear", false, false},
		{"a member who left by hand", "pid-7-b", "clear", true, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, _, old, _, _ := clearFixture(t, c.alive)
			if c.left {
				wantOK(t, f.run(old, Leave{}, ""))
			}
			fresh := f.newSession("claude")
			f.hook(fresh, start(c.launch, c.source, f.now.UnixMilli()+20))
			if f.keyState(old) != "live" {
				t.Fatal("the old key was retired")
			}
			var principal int64
			if err := f.db.QueryRow(`SELECT principal_id FROM principal_key WHERE key = ?`, fresh.key).Scan(&principal); err != nil {
				t.Fatal(err)
			}
			if principal == *f.caller(old).PrincipalID {
				t.Fatal("the new session took over the old member")
			}
		})
	}
}

func joinLaunch(ref string, at int64) *JoinLaunch {
	return &JoinLaunch{Ref: ref, AtMS: at, Profile: testProfile}
}

// #622: a hand join inside a running session binds its process, so its
// first /clear keeps the member too.
func TestClearAfterAHandJoinKeepsTheMember(t *testing.T) {
	f, writer := adapterChannel(t)
	f.svc.LaunchProcess = testProcess
	f.svc.LaunchAlive = func(string, int64) bool { return true }
	old := f.newSession("claude")
	r := decode[JoinResult](t, f.join(old, Join{Channel: "checkout", Alias: "worker",
		Launch: joinLaunch("pid-7-join", f.now.UnixMilli())}, ""))
	if r.Launch != "pid-7-join" {
		t.Fatalf("the join bound %q, want its launch", r.Launch)
	}
	unread := decode[AskResult](t, f.ask(writer, Ask{To: []string{"worker"}, Body: "unread"}, "k1")).ID
	cleared := f.newSession("claude")
	got := f.hook(cleared, start("pid-7-b", "clear", f.now.UnixMilli()+20))
	if got.Enrollment != "restored" || got.Alias != "worker" || f.keyState(old) != "retired" {
		t.Fatalf("clear after a hand join: %+v", got)
	}
	if inbox := decode[InboxResult](t, f.run(cleared, Inbox{}, "")); len(inbox.Unread) != 1 || inbox.Unread[0].ID != unread {
		t.Fatalf("the cleared session's inbox: %+v", inbox)
	}
}

func TestAJoinBindsOnlyARunningProcessNotYetBound(t *testing.T) {
	for _, c := range []struct {
		name    string
		alive   bool
		started bool
		adapter string
		at      int64
	}{
		{name: "a dead process", alive: false, adapter: "claude"},
		{name: "a process a start already bound", alive: true, started: true, adapter: "claude"},
		{name: "another adapter", alive: true, adapter: "codex"},
		{name: "an old time", alive: true, adapter: "claude", at: -joinLaunchAgeMS - 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, _ := adapterChannel(t)
			f.svc.LaunchProcess = testProcess
			f.svc.LaunchAlive = func(string, int64) bool { return c.alive }
			s := f.newSession("claude")
			if c.started {
				f.hook(s, start("pid-7-a", "startup", f.now.UnixMilli()))
			}
			l := joinLaunch("pid-7-join", f.now.UnixMilli()+c.at)
			l.Profile.Adapter = c.adapter
			alias := "worker"
			if c.started {
				alias = "claude1"
			}
			res := f.join(s, Join{Channel: "checkout", Alias: alias, Launch: l}, "")
			wantOK(t, res)
			r := decode[JoinResult](t, res)
			if r.Launch != "" || f.count(`SELECT COUNT(*) FROM adapter_launch WHERE launch_ref = 'pid-7-join'`) != 0 {
				t.Fatalf("the join bound %q", r.Launch)
			}
		})
	}
}
