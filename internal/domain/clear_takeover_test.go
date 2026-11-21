package domain

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
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
		{"a session nested under the process", "pid-7-b", "nested", true, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, _, old, _, _ := clearFixture(t, c.alive)
			if c.left {
				wantOK(t, f.run(old, Leave{}, ""))
			}
			fresh := f.newSession("claude")
			cmd := start(c.launch, c.source, f.now.UnixMilli()+20)
			if c.source == "nested" {
				cmd.Source, cmd.LaunchHosted = "clear", false
			}
			f.hook(fresh, cmd)
			if f.keyState(old) != "live" {
				t.Fatal("the old key was retired")
			}
			var principal int64
			err := f.db.QueryRow(`SELECT principal_id FROM principal_key WHERE key = ?`, fresh.key).Scan(&principal)
			if err != nil && err != sql.ErrNoRows {
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
	if r.Launch == nil || r.Launch.Ref != "pid-7-join" {
		t.Fatalf("the join bound %+v, want its launch", r.Launch)
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
			if r.Launch != nil || f.count(`SELECT COUNT(*) FROM adapter_launch WHERE launch_ref = 'pid-7-join'`) != 0 {
				t.Fatalf("the join bound %+v", r.Launch)
			}
		})
	}
}

// Fable #640 F1: the cleared conversation's end may carry a hook time at or
// after the clear's start, and still be applied first.
func TestClearKeepsTheMemberWhenTheEndIsNotOlder(t *testing.T) {
	for _, gap := range []int64{0, 5} {
		t.Run(fmt.Sprintf("end %d ms after the start", gap), func(t *testing.T) {
			f, writer, old, owed, unread := clearFixture(t, true)
			at := f.now.UnixMilli() + 20
			f.hook(old, ended("pid-7-a", at+gap))
			cleared := f.newSession("claude")
			r := f.hook(cleared, start("pid-7-b", "clear", at))
			if r.Enrollment != "restored" || r.Alias != "claude1" {
				t.Fatalf("clear start: %+v, want claude1 restored", r)
			}
			wantTakenOver(t, f, writer, old, cleared, owed, unread, r.Cue+"\n"+r.PendingLine)
		})
	}
}

// Fable #640 F3 and the lead's O2: the process's latest launch decides. Its
// member left by hand or was departed by the owner, so an older member of
// the process is not revived and the clear joins nobody.
func TestClearAfterTheProcessMemberLeftJoinsNobody(t *testing.T) {
	for _, how := range []string{"by hand", "by the owner"} {
		t.Run(how, func(t *testing.T) {
			f, _, first, _, _ := clearFixture(t, true)
			f.hook(first, ended("pid-7-a", f.now.UnixMilli()+10))
			second := f.newSession("claude")
			f.hook(second, start("pid-7-b", "resume", f.now.UnixMilli()+20))
			if how == "by hand" {
				wantOK(t, f.run(second, Leave{}, ""))
			} else if _, err := f.db.Exec(`UPDATE binding SET state = 'departed', departed_by_owner = 1, departed_by_hook = 0,
				owner_hook_fence_ms = ? WHERE principal_id = ?`, f.now.UnixMilli()+25, *f.caller(second).PrincipalID); err != nil {
				t.Fatal(err)
			}
			bindings := f.count(`SELECT COUNT(*) FROM binding`)
			cleared := f.newSession("claude")
			if r := f.hook(cleared, start("pid-7-c", "clear", f.now.UnixMilli()+30)); r.Enrollment != "none" || r.Alias != "" {
				t.Fatalf("clear start: %+v, want nothing", r)
			}
			if f.keyState(first) != "live" || f.keyState(second) != "live" || f.count(`SELECT COUNT(*) FROM binding`) != bindings {
				t.Fatal("the clear revived or added a member")
			}
		})
	}
}

// Fable #642 G1: a keyed join retried proposes a new launch; it is the same
// join and replays the launch the first call bound.
func TestARetriedJoinReplaysItsLaunch(t *testing.T) {
	f, _ := adapterChannel(t)
	f.svc.LaunchProcess = testProcess
	f.svc.LaunchAlive = func(string, int64) bool { return true }
	s := f.newSession("claude")
	first := decode[JoinResult](t, f.join(s, Join{Channel: "checkout", Alias: "worker", Launch: joinLaunch("pid-7-j1", f.now.UnixMilli())}, "k"))
	again := f.join(s, Join{Channel: "checkout", Alias: "worker", Launch: joinLaunch("pid-7-j2", f.now.UnixMilli()+5)}, "k")
	wantOK(t, again)
	if r := decode[JoinResult](t, again); first.Launch == nil || r.Launch == nil || r.Launch.Ref != "pid-7-j1" {
		t.Fatalf("retried join bound %+v, want the first call's %+v", r.Launch, first.Launch)
	}
	if n := f.count(`SELECT COUNT(*) FROM adapter_launch WHERE launch_ref = 'pid-7-j2'`); n != 0 {
		t.Fatal("the retry bound a second launch")
	}
}

// Fable #642 G2: one process runs one conversation, whichever member bound it.
func TestAJoinDoesNotBindAProcessAnotherMemberRuns(t *testing.T) {
	for _, how := range []string{"started", "joined by hand"} {
		t.Run(how, func(t *testing.T) {
			f, _ := adapterChannel(t)
			f.svc.LaunchProcess = testProcess
			f.svc.LaunchAlive = func(string, int64) bool { return true }
			first := f.newSession("claude")
			if how == "started" {
				f.hook(first, start("pid-7-a", "startup", f.now.UnixMilli()))
			} else {
				wantOK(t, f.join(first, Join{Channel: "checkout", Alias: "first", Launch: joinLaunch("pid-7-j1", f.now.UnixMilli())}, ""))
			}
			second := f.newSession("claude")
			r := decode[JoinResult](t, f.join(second, Join{Channel: "checkout", Alias: "second", Launch: joinLaunch("pid-7-j2", f.now.UnixMilli()+5)}, ""))
			if r.Launch != nil || f.count(`SELECT COUNT(*) FROM adapter_launch WHERE launch_ref = 'pid-7-j2'`) != 0 {
				t.Fatalf("a second member bound the process: %+v", r.Launch)
			}
		})
	}
}

// recoveryPrompt is the first prompt of a session no start enrolled, which
// may join the repository's channel.
func recoveryPrompt(launch, turn string, at int64) HookEvent {
	cmd := event(launch, FactPromptSubmitted, turn+":UserPromptSubmit", turn, at, true)
	cmd.EnrollRepoRoot = testRepo
	return cmd
}

// Astra #668: after a hand leave and a clear, the new conversation's first
// prompt stays out too, until it joins explicitly; then a later clear
// continues the member it joined as.
func TestAfterAHandLeaveTheProcessJoinsOnlyExplicitly(t *testing.T) {
	f, _, first, _, _ := clearFixture(t, true)
	wantOK(t, f.run(first, Leave{}, ""))
	members := f.count(`SELECT COUNT(*) FROM binding`)
	cleared := f.newSession("claude")
	f.hook(cleared, start("pid-7-b", "clear", f.now.UnixMilli()+20))
	f.hook(cleared, recoveryPrompt("pid-7-b", "first", f.now.UnixMilli()+30))
	if n := f.count(`SELECT COUNT(*) FROM binding`); n != members {
		t.Fatalf("the cleared conversation's prompt joined a member: %d bindings, want %d", n, members)
	}
	r := decode[JoinResult](t, f.join(cleared, Join{Channel: "checkout", Alias: "again",
		Launch: joinLaunch("pid-7-join", f.now.UnixMilli()+40)}, ""))
	if r.Launch == nil {
		t.Fatal("the explicit join did not bind the process its absent member left")
	}
	next := f.newSession("claude")
	if got := f.hook(next, start("pid-7-c", "clear", f.now.UnixMilli()+50)); got.Enrollment != "restored" || got.Alias != "again" {
		t.Fatalf("a later clear: %+v, want @again continued", got)
	}
}

// Astra #670: an owner restore moves the member to a new key; a /clear in
// the old conversation's process does not take it back.
func TestAClearAfterAnOwnerRestoreDoesNotTakeTheMemberBack(t *testing.T) {
	f, _, old, _, _ := clearFixture(t, true)
	f.svc.RestoreKey = func(ctx context.Context, tx store.Tx, principal int64, vendor, key string, now time.Time) (bool, error) {
		if _, err := tx.ExecContext(ctx, `UPDATE principal_key SET state = 'retired', retired_at_ms = ? WHERE principal_id = ?`,
			now.UnixMilli(), principal); err != nil {
			return false, err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO principal_key (vendor, key, principal_id, created_at_ms) VALUES (?, ?, ?, 0)`,
			vendor, key, principal)
		return err == nil, err
	}
	principal := *f.caller(old).PrincipalID
	binding := int64(f.count(`SELECT id FROM binding WHERE principal_id = ?`, principal))
	replacement := f.newSession("claude")
	wantOK(t, f.owner(Restore{BindingID: binding, Vendor: replacement.vendor, Key: replacement.key}, "restore"))
	cleared := f.newSession("claude")
	f.hook(cleared, start("pid-7-b", "clear", f.now.UnixMilli()+20))
	if f.keyState(replacement) != "live" {
		t.Fatal("a clear in the old process retired the restored key")
	}
	var got int64
	if err := f.db.QueryRow(`SELECT principal_id FROM principal_key WHERE key = ?`, cleared.key).Scan(&got); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	if got == principal {
		t.Fatal("a clear in the old process took the restored member back")
	}
}
