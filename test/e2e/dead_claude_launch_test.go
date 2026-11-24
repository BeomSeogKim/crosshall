//go:build e2e

package e2e

import (
	"database/sql"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/proc"
)

func TestDispatcherRetiresKilledClaudeLaunch(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("requires kernel process identity")
	}
	t.Run("known identity", func(t *testing.T) { checkKilledClaudeLaunch(t, true, "") })
	t.Run("died before identity was stored", func(t *testing.T) { checkKilledClaudeLaunch(t, false, "") })
	// The stored launch's revision decides its exit authority (#326): a
	// prior revision that declares process_launch ends by its process; one
	// this build does not know does not, whatever its reference.
	t.Run("prior revision", func(t *testing.T) { checkKilledClaudeLaunch(t, true, "claude-1") })
	t.Run("unknown revision", func(t *testing.T) { checkKilledClaudeLaunch(t, true, "claude-9") })
}

func TestDispatcherRetiresKilledHandJoinedClaudeLaunchOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires a Linux PID namespace")
	}
	namespace := proc.PIDNamespace()
	if namespace == "" {
		t.Fatal("cannot read the test process's PID namespace")
	}
	w, writer, c := m2World(t)
	process := exec.CommandContext(t.Context(), "/bin/sh", "-c", `export CLAUDE_PID=$$
"$1" join checkout --as hand || exit $?
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

	type launchState struct {
		binding, pidNamespace          string
		pid, startIdentity             int64
		departedByHook, endedByProcess bool
	}
	db := w.db()
	state := func() launchState {
		t.Helper()
		var got launchState
		err := db.QueryRow(`SELECT b.state, l.pid_namespace, COALESCE(l.process_pid, 0),
			COALESCE(l.process_start_identity, 0), b.departed_by_hook, l.ended_by_process
			FROM principal_key k JOIN binding b ON b.principal_id = k.principal_id
			JOIN adapter_launch l ON l.principal_id = k.principal_id
			WHERE k.vendor = 'claude' AND k.key = ?`, c.key).Scan(&got.binding, &got.pidNamespace, &got.pid,
			&got.startIdentity, &got.departedByHook, &got.endedByProcess)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
		return got
	}
	for deadline := time.Now().Add(fixtureEventTimeout); ; time.Sleep(25 * time.Millisecond) {
		got := state()
		if got.binding == "live" {
			if got.pidNamespace != namespace {
				t.Fatalf("hand join stored PID namespace %q, want %q", got.pidNamespace, namespace)
			}
			if got.pid == int64(process.Process.Pid) && got.startIdentity > 0 {
				break
			}
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("hand join did not bind a live launch: %+v", got)
		}
	}
	if err := process.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := process.Wait(); err == nil {
		t.Fatal("expected the fixture process to exit after SIGKILL")
	}
	for deadline := time.Now().Add(fixtureEventTimeout); ; time.Sleep(25 * time.Millisecond) {
		got := state()
		if got.binding == "departed" && got.departedByHook && got.endedByProcess {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("dispatcher did not retire the hand-joined launch: %+v", got)
		}
	}
	if rosterHas(t, writer, "hand") {
		t.Fatal("the killed hand-joined launch remains live in who")
	}
}

func checkKilledClaudeLaunch(t *testing.T, knownIdentity bool, revision string) {
	t.Helper()
	w, writer, c := m2World(t)
	process := exec.CommandContext(t.Context(), "/bin/sleep", "60")
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = process.Process.Kill()
		_ = process.Wait()
	})
	pid := process.Process.Pid
	kill := func() {
		t.Helper()
		if err := process.Process.Kill(); err != nil {
			t.Fatal(err)
		}
		if err := process.Wait(); err == nil {
			t.Fatal("expected the fixture process to exit after SIGKILL")
		}
	}
	if !knownIdentity {
		kill()
	}
	start := fmt.Sprintf(`{"hook_event_name":"SessionStart","session_id":%q,"source":"startup","cwd":%q,"permission_mode":"default"}`, c.key, c.repo)
	fireStart := func() {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), binary, "hook", "claude", "SessionStart")
		cmd.Dir = c.repo
		cmd.Stdin = strings.NewReader(start)
		cmd.Env = append(w.baseEnv(), "CLAUDE_CODE_SESSION_ID="+c.key,
			fmt.Sprintf("CLAUDE_PID=%d", pid), fmt.Sprintf("CLAUDE_CODE_MESSAGING_SOCKET=/tmp/cc-socks/%d.sock", pid))
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("start fixture hook: %v, output %q", err, out)
		}
	}

	type launchState struct {
		binding, route, pidNamespace   string
		generation, pid, startIdentity int64
		departedByHook, endedByProcess bool
	}
	db := w.db()
	state := func() launchState {
		t.Helper()
		var got launchState
		err := db.QueryRow(`SELECT b.state, r.state, l.pid_namespace, r.generation, COALESCE(l.process_pid, 0),
			COALESCE(l.process_start_identity, 0), b.departed_by_hook, l.ended_by_process
			FROM binding b JOIN publish_route r ON r.binding_id = b.id
			JOIN adapter_launch l ON l.principal_id = r.principal_id AND l.launch_ref = r.launch_ref
			WHERE r.session_ref = ?`, c.key).Scan(&got.binding, &got.route, &got.pidNamespace, &got.generation, &got.pid,
			&got.startIdentity, &got.departedByHook, &got.endedByProcess)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			t.Fatal(err)
		}
		return got
	}
	digest := (ident.Identity{Vendor: "claude", Key: c.key}).Digest()
	first := filepath.Join(w.rendezvous(), "first")
	member := filepath.Join(w.rendezvous(), "m", digest[:16])
	var before launchState
	for deadline := time.Now().Add(fixtureEventTimeout); ; time.Sleep(25 * time.Millisecond) {
		before = state()
		if knownIdentity && before.binding == "live" && before.route == "active" && before.pid == int64(pid) && before.startIdentity > 0 {
			break
		}
		if !knownIdentity && before.binding != "" {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("launch was not registered (expect kernel identity: %t): %+v", knownIdentity, before)
		}
		if before.route != "active" && len(fixtureQueuedPackets(t, first)) == 0 && len(fixtureQueuedPackets(t, member)) == 0 {
			fireStart()
		}
	}
	if runtime.GOOS == "linux" && before.pidNamespace == "" {
		t.Fatal("Linux startup did not persist its PID namespace")
	}
	if revision != "" {
		writable, err := sql.Open("sqlite", "file:"+filepath.Join(w.state, "store.sqlite3")+"?mode=rw&_pragma=busy_timeout(5000)")
		if err != nil {
			t.Fatal(err)
		}
		defer writable.Close()
		if _, err := writable.Exec(`UPDATE adapter_launch SET profile = json_set(profile, '$.manifest_rev', ?)
			WHERE launch_ref GLOB 'pid-*' AND json_extract(profile, '$.adapter') = 'claude'`, revision); err != nil {
			t.Fatal(err)
		}
	}
	var id int64
	if knownIdentity {
		id = num(t, writer.must("ask", "@claude1", "pending when the process exits"), "id")
		kill()
	}
	if revision == "claude-9" {
		time.Sleep(4 * launchSweepIntervalForTest)
		if got := state(); got.endedByProcess || got.binding != "live" {
			t.Fatalf("a launch of an unknown revision was ended by its process: %+v", got)
		}
		return
	}
	for deadline := time.Now().Add(fixtureEventTimeout); ; time.Sleep(25 * time.Millisecond) {
		got := state()
		if got.binding == "departed" && got.departedByHook && got.endedByProcess && got.route == "ended" {
			if knownIdentity && got.generation <= before.generation {
				t.Fatalf("retired route kept generation %d", got.generation)
			}
			if !knownIdentity && (got.pid != 0 || got.startIdentity != 0) {
				t.Fatalf("the stopped launch unexpectedly obtained a process identity: %+v", got)
			}
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("dispatcher did not retire the killed launch: %+v", got)
		}
	}
	if rosterHas(t, writer, "claude1") {
		t.Fatal("the killed Claude launch remains live in who")
	}
	if out := writer.run("ask", "@claude1", "the process is gone"); out.obj["code"] != "departed" {
		t.Fatalf("ask to the retired launch: %s", out.stdout)
	}
	if knownIdentity {
		assertWorkUnread(t, db, id)
	}
	var attempts int
	if err := db.QueryRow(`SELECT COUNT(*) FROM publish_attempt`).Scan(&attempts); err != nil || attempts != 0 {
		t.Fatalf("publish attempts after retiring killed fixture: %d (%v)", attempts, err)
	}
}

// launchSweepIntervalForTest is the dispatcher's launch sweep interval.
const launchSweepIntervalForTest = time.Second
