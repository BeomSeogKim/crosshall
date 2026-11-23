//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

const namedHostSource = `package main

import (
	"fmt"
	"os"
	"os/exec"
	"time"
)

func main() {
	args := os.Args[1:]
	if args[0] == "forward" {
		args = args[1:]
	} else {
		args = append([]string{args[0], "forward"}, args[1:]...)
	}
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if os.Args[1] != "forward" {
		time.Sleep(time.Minute)
	}
}
`

func TestNamedAntigravityHostLifecycle(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("requires kernel process names and identity")
	}
	agy, relay, _ := namedHostHelpers(t)
	w := newWorld(t)
	owner := w.owner()
	owner.must("owner", "channel", "create", "checkout")
	author := w.member()
	author.must("join", "checkout", "--as", "writer")
	host, _ := runNamedHost(t, w, agy, relay, "ANTIGRAVITY_CONVERSATION_ID=agy-test",
		"join", "checkout", "--as", "antigravity")
	db := w.db()
	state := func() hostState { return memberHostState(t, db, "b.alias = ?", "antigravity") }
	awaitHost(t, host, state)

	ask := author.must("ask", "@antigravity", "keep this request across the process exit", "--due", "10m")
	requestID := num(t, ask, "id")
	if err := host.Process.Signal(syscall.SIGSTOP); err != nil {
		t.Fatal(err)
	}
	time.Sleep(3200 * time.Millisecond)
	out := owner.mustPlain("owner", "who", "--all")
	if got := state(); got.binding != "live" || !strings.Contains(out, "live, alive, agy") ||
		!strings.Contains(out, "ends with its process") {
		t.Fatalf("the suspended host should remain alive: %+v\n%s", got, out)
	}
	if err := host.Process.Signal(syscall.SIGCONT); err != nil {
		t.Fatal(err)
	}
	killAndAwaitDeparture(t, host, state)
	if got := pendingDepartureRecipient(t, author, requestID); got["binding_state"] != "departed" {
		t.Fatalf("open request did not retain the departed member: %v", got)
	}

	resumed, outPath := runNamedHost(t, w, agy, relay, "ANTIGRAVITY_CONVERSATION_ID=agy-test",
		"--json", "whoami", "--full")
	awaitHost(t, resumed, state)
	var identity map[string]any
	for deadline := time.Now().Add(fixtureEventTimeout); ; time.Sleep(25 * time.Millisecond) {
		raw, err := os.ReadFile(outPath)
		if err == nil && json.Unmarshal(raw, &identity) == nil {
			break
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("no JSON identity from resumed host: %s, %v", raw, err)
		}
	}
	if identity["enrollment"] != "enrolled" || identity["exit_tracking"] != "process" {
		t.Fatalf("resumed identity did not report process tracking: %v", identity)
	}
	if returned := liveDepartureBinding(t, owner, "antigravity"); !slices.Contains(returned.OpenRequestIDs, requestID) {
		t.Fatalf("resumed host lost its original request: %+v", returned)
	}
}

func TestCodexProcessNameAloneLeavesExitUntracked(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("requires kernel process names")
	}
	_, relay, codex := namedHostHelpers(t)
	w := newWorld(t)
	owner := w.owner()
	owner.must("owner", "channel", "create", "checkout")
	host, _ := runNamedHost(t, w, codex, relay, "CODEX_THREAD_ID=codex-test", "join", "checkout", "--as", "codex")
	db := w.db()
	state := func() hostState { return memberHostState(t, db, "b.alias = ?", "codex") }
	for deadline := time.Now().Add(fixtureEventTimeout); state().binding != "live"; time.Sleep(25 * time.Millisecond) {
		if !time.Now().Before(deadline) {
			t.Fatal("the Codex fixture did not join")
		}
	}
	if got := state(); got.hostPID != 0 {
		t.Fatalf("a name without session ownership recorded a Codex host: %+v", got)
	}
	if out := owner.mustPlain("owner", "who", "--all"); !strings.Contains(out, "exit not tracked") {
		t.Fatalf("the owner should see the untracked exit: %s", out)
	}
	if err := host.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = host.Wait()
	time.Sleep(1200 * time.Millisecond)
	if got := state(); got.binding != "live" {
		t.Fatalf("the unknown Codex host was treated as a session end: %+v", got)
	}
}

func namedHostHelpers(t *testing.T) (agy, relay, codex string) {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(namedHostSource), 0600); err != nil {
		t.Fatal(err)
	}
	agy = filepath.Join(dir, "agy")
	if raw, err := exec.CommandContext(t.Context(), "go", "build", "-o", agy, src).CombinedOutput(); err != nil {
		t.Fatalf("build host helper: %v\n%s", err, raw)
	}
	content, err := os.ReadFile(agy)
	if err != nil {
		t.Fatal(err)
	}
	relay, codex = filepath.Join(dir, "relay"), filepath.Join(dir, "codex")
	if err := os.WriteFile(relay, content, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(codex, content, 0700); err != nil {
		t.Fatal(err)
	}
	return agy, relay, codex
}

func runNamedHost(t *testing.T, w *world, host, relay, identity string, args ...string) (*exec.Cmd, string) {
	t.Helper()
	out, err := os.CreateTemp(t.TempDir(), "host-output-")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	cmd := exec.CommandContext(t.Context(), host, append([]string{relay, binary}, args...)...)
	cmd.Env = append(w.baseEnv(), identity)
	cmd.Dir = w.home
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	return cmd, out.Name()
}
