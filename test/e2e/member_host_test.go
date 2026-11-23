//go:build e2e

package e2e

import (
	"database/sql"
	"errors"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

// hostState is what the store says about the member a session key or an
// alias names.
type hostState struct {
	binding        string
	departedByHook bool
	hostPID        int64
	launches       int
}

func memberHostState(t *testing.T, db *sql.DB, where string, arg any) hostState {
	t.Helper()
	var got hostState
	err := db.QueryRow(`SELECT b.state, b.departed_by_hook, COALESCE(h.pid, 0),
		(SELECT COUNT(*) FROM adapter_launch l WHERE l.principal_id = b.principal_id)
		FROM binding b LEFT JOIN member_host h ON h.principal_id = b.principal_id WHERE `+where, arg).
		Scan(&got.binding, &got.departedByHook, &got.hostPID, &got.launches)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		t.Fatal(err)
	}
	return got
}

// killAndAwaitDeparture kills the session's process and waits for the sweep
// to depart its member as a clean session end.
func killAndAwaitDeparture(t *testing.T, process *exec.Cmd, state func() hostState) {
	t.Helper()
	if err := process.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := process.Wait(); err == nil {
		t.Fatal("expected the session process to exit after SIGKILL")
	}
	for deadline := time.Now().Add(fixtureEventTimeout); ; time.Sleep(25 * time.Millisecond) {
		got := state()
		if got.binding == "departed" && got.departedByHook && got.hostPID == 0 {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("the dispatcher did not depart the member of the ended process: %+v", got)
		}
	}
}

func awaitHost(t *testing.T, process *exec.Cmd, state func() hostState) hostState {
	t.Helper()
	for deadline := time.Now().Add(fixtureEventTimeout); ; time.Sleep(25 * time.Millisecond) {
		got := state()
		if got.binding == "live" && got.hostPID == int64(process.Process.Pid) {
			return got
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("the member's host was not recorded: %+v, want pid %d", got, process.Process.Pid)
		}
	}
}

// A member started with crosshall run departs when the command it runs
// ends, and its alias is free again.
func TestCrosshallRunMemberDepartsWhenItsCommandEnds(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("requires kernel process identity")
	}
	w := newWorld(t)
	owner := w.owner()
	owner.must("owner", "start")
	owner.must("owner", "channel", "create", "checkout")
	writer := w.member()
	writer.must("join", "checkout", "--as", "writer")
	process := exec.CommandContext(t.Context(), binary, "run", "--", "/bin/sh", "-c", `"$0" join checkout --as runner || exit $?
exec /bin/sleep 60`, binary)
	process.Env = w.baseEnv()
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = process.Process.Kill()
		_ = process.Wait()
	})
	db := w.db()
	state := func() hostState { return memberHostState(t, db, `b.alias = ?`, "runner") }
	awaitHost(t, process, state)
	killAndAwaitDeparture(t, process, state)
	if rosterHas(t, writer, "runner") {
		t.Fatal("the ended member remains live in who")
	}
	w.member().must("join", "checkout", "--as", "runner")
}

// A Claude member whose join bound no launch is tracked from its next
// command that names the Claude process, and departs when that process ends.
func TestClaudeMemberWithoutALaunchDepartsWithItsProcess(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("requires kernel process identity")
	}
	w, writer, c := m2World(t)
	process := exec.CommandContext(t.Context(), "/bin/sh", "-c", `unset CLAUDE_PID
"$1" join checkout --as hand || exit $?
export CLAUDE_PID=$$
"$1" who >/dev/null || exit $?
exec /bin/sleep 60`, "sh", binary)
	process.Dir = c.repo
	process.Env = append(w.baseEnv(), "CLAUDE_CODE_SESSION_ID="+c.key)
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = process.Process.Kill()
		_ = process.Wait()
	})
	db := w.db()
	state := func() hostState {
		return memberHostState(t, db, `b.principal_id = (SELECT principal_id FROM principal_key WHERE vendor = 'claude' AND key = ?)`, c.key)
	}
	if got := awaitHost(t, process, state); got.launches != 0 {
		t.Fatalf("the join bound a launch: %+v", got)
	}
	killAndAwaitDeparture(t, process, state)
	if rosterHas(t, writer, "hand") {
		t.Fatal("the ended member remains live in who")
	}
	// The session resumes in a new process: its first command brings the
	// member back under its alias, without a join.
	resumed := exec.CommandContext(t.Context(), "/bin/sh", "-c", `export CLAUDE_PID=$$
"$1" inbox >/dev/null || exit $?
exec /bin/sleep 60`, "sh", binary)
	resumed.Dir = c.repo
	resumed.Env = append(w.baseEnv(), "CLAUDE_CODE_SESSION_ID="+c.key)
	if err := resumed.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = resumed.Process.Kill()
		_ = resumed.Wait()
	})
	awaitHost(t, resumed, state)
	if !rosterHas(t, writer, "hand") {
		t.Fatal("the resumed member is not back as @hand")
	}
}

// A member whose hook start opened a launch nothing will end (a Claude Code
// start without CLAUDE_PID is session-scoped, as Antigravity's is) is still
// tracked by its host, and that launch ends with the member.
func TestMemberWithAnUnendingLaunchDepartsWithItsProcess(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("requires kernel process identity")
	}
	w, writer, c := m2World(t)
	c.pid = ""
	join := exec.CommandContext(t.Context(), binary, "join", "checkout", "--as", "hand")
	join.Dir = c.repo
	join.Env = append(w.baseEnv(), "CLAUDE_CODE_SESSION_ID="+c.key)
	if out, err := join.CombinedOutput(); err != nil {
		t.Fatalf("join: %v\n%s", err, out)
	}
	db := w.db()
	openLaunches := func() (n int) {
		if err := db.QueryRow(`SELECT COUNT(*) FROM adapter_launch WHERE launch_ref = 'session' AND ended_hook_at_ms IS NULL`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	for deadline := time.Now().Add(fixtureEventTimeout); openLaunches() == 0; time.Sleep(25 * time.Millisecond) {
		if !time.Now().Before(deadline) {
			t.Fatal("the start opened no session launch")
		}
		c.fire("SessionStart", map[string]any{"source": "startup"})
	}
	process := exec.CommandContext(t.Context(), "/bin/sh", "-c", `export CLAUDE_PID=$$
"$1" who >/dev/null || exit $?
exec /bin/sleep 60`, "sh", binary)
	process.Dir = c.repo
	process.Env = append(w.baseEnv(), "CLAUDE_CODE_SESSION_ID="+c.key)
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = process.Process.Kill()
		_ = process.Wait()
	})
	state := func() hostState {
		return memberHostState(t, db, `b.principal_id = (SELECT principal_id FROM principal_key WHERE vendor = 'claude' AND key = ?)`, c.key)
	}
	awaitHost(t, process, state)
	killAndAwaitDeparture(t, process, state)
	if n := openLaunches(); n != 0 {
		t.Fatalf("open session launches after the departure: %d", n)
	}
	if rosterHas(t, writer, "hand") {
		t.Fatal("the ended member remains live in who")
	}
}
