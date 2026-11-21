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
)

func TestDispatcherRetiresKilledClaudeLaunch(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("requires kernel process identity")
	}
	t.Run("known identity", func(t *testing.T) { checkKilledClaudeLaunch(t, true) })
	t.Run("died before identity was stored", func(t *testing.T) { checkKilledClaudeLaunch(t, false) })
}

func checkKilledClaudeLaunch(t *testing.T, knownIdentity bool) {
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
	grantRoots(t, w, "writer", 2)
	var id int64
	if knownIdentity {
		id = num(t, writer.must("ask", "@claude1", "pending when the process exits"), "id")
		kill()
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
