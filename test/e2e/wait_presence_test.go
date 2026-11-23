//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/proc"
)

// TestSendLineShowsARunningWait: the sender's line says the recipient's wait
// is running while it polls, and goes back to pull once the wait is killed.
func TestSendLineShowsARunningWait(t *testing.T) {
	_, _, members := setup(t)
	form, tax := members[0], members[1]
	next := func(key string) string {
		t.Helper()
		out := tax.must("--key", key, "ask", "@form", "work", "--due", "10m")
		recipients, _ := out.obj["recipients"].([]any)
		if len(recipients) != 1 {
			t.Fatalf("ask recipients: %v", out.obj)
		}
		next, _ := recipients[0].(map[string]any)["next"].(string)
		return next
	}
	waitRunning := func() bool {
		t.Helper()
		for _, m := range tax.must("who").obj["members"].([]any) {
			if row := m.(map[string]any); row["alias"] == "form" {
				return row["wake"] == "wait"
			}
		}
		t.Fatal("who lists no @form")
		return false
	}

	child := startWait(t, form, "--work-only")
	requireWaiting(t, child, 500*time.Millisecond)
	if !waitRunning() {
		t.Error("who does not show @form's running wait")
	}
	if got := next("while-waiting"); got != domain.WaitRunning {
		t.Errorf("send line while @form waits: %q", got)
	}
	awaitWaitMail(t, child)

	process, killed := startWaitProcess(t, form, "--work-only")
	requireWaiting(t, killed, 500*time.Millisecond)
	if err := process.Kill(); err != nil {
		t.Fatal(err)
	}
	<-killed
	time.Sleep(3500 * time.Millisecond) // past the three seconds a poll stays fresh
	if waitRunning() {
		t.Error("who shows a killed wait")
	}
	if got := next("after-kill"); got != "waits for its next crosshall command" {
		t.Errorf("send line after the wait was killed: %q", got)
	}
}

// TestWaitEndsWithItsAgent: a wait whose named agent process ends exits by
// itself instead of polling for nobody.
func TestWaitEndsWithItsAgent(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("requires kernel process identity")
	}
	w, _, members := setup(t)
	form := members[0]
	// The shell reads its own host value, then runs the wait as its child.
	// The wait is bounded, so a broken host check fails with a timeout and
	// leaves no process behind.
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", `read host; CROSSHALL_HOST=$host "$0" --json wait --work-only --timeout 12s; echo unreachable`, binary)
	cmd.Dir, cmd.Env = w.home, form.env()
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	start, err := proc.StartOf(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(stdin, "%d:%d\n", cmd.Process.Pid, start.Identity)
	time.Sleep(time.Second) // the wait is polling
	killed := time.Now()
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait() // returns once the orphaned wait closes the output
	if elapsed := time.Since(killed); elapsed > 10*time.Second {
		t.Errorf("the wait ran %s after its agent ended", elapsed)
	}
	if got := out.String(); !strings.Contains(got, `"code":"wait_host_gone"`) || strings.Contains(got, "unreachable") {
		t.Errorf("wait output: %q", got)
	}
}
