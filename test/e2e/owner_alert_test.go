//go:build e2e

package e2e

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A refused request returns while the desktop notification for its stop is
// still running.
func TestRefusedRequestDoesNotWaitForTheNotification(t *testing.T) {
	w := newWorld(t)
	dir := t.TempDir()
	started, command := filepath.Join(dir, "started"), filepath.Join(dir, "notify")
	script := fmt.Sprintf("#!/bin/sh\necho $$ > '%s.tmp' && mv '%[1]s.tmp' '%[1]s'\nexec sleep 20\n", started)
	if err := os.WriteFile(command, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	owner := w.owner()
	owner.extra = []string{"CROSSHALL_TEST_NOTIFY_COMMAND=" + command}
	owner.must("owner", "start")
	owner.must("owner", "channel", "create", "checkout")
	form, tax := w.member(), w.member()
	form.mustFixtureWrite("join", "checkout", "--as", "form")
	tax.mustFixtureWrite("join", "checkout", "--as", "tax")
	owner.must("owner", "budget", "grant", "checkout/@form", "--rounds", "0")
	if o := form.run("ask", "@tax", "--new", "first"); o.exit != 3 || o.obj["code"] != "round_limit" {
		t.Fatalf("first refusal: exit %d %v", o.exit, o.obj)
	}

	var pid int
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(50 * time.Millisecond) {
		if raw, err := os.ReadFile(started); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
			break
		}
		if time.Now().After(deadline) {
			w.logDispatcher()
			t.Fatal("the notification command did not start")
		}
	}
	t.Cleanup(func() { _ = syscall.Kill(pid, syscall.SIGKILL) })

	begin := time.Now()
	o := form.run("ask", "@tax", "--new", "second")
	if elapsed := time.Since(begin); o.exit != 3 || o.obj["code"] != "round_limit" || elapsed > 3*time.Second {
		t.Fatalf("second refusal took %v: exit %d %v", elapsed, o.exit, o.obj)
	}
	var state string
	if err := w.db().QueryRow(`SELECT desktop_state FROM owner_alert`).Scan(&state); err != nil || state != "sending" {
		t.Fatalf("notification state %q (%v), want sending", state, err)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("the notification command already ended: %v", err)
	}
}

// The owner's next command prints an open stop once, on stderr, leaving
// its JSON output and exit status as they were.
func TestOwnerCommandPrintsAStopOnceOnStderr(t *testing.T) {
	_, owner, members := setup(t)
	form := members[0]
	owner.must("owner", "budget", "grant", "checkout/@form", "--rounds", "0")
	if o := form.run("ask", "@tax", "--new", "stopped"); o.exit != 3 || o.obj["code"] != "round_limit" ||
		strings.Contains(o.stderr, "budget grant") {
		t.Fatalf("refusal: exit %d stderr %q %v", o.exit, o.stderr, o.obj)
	}
	first := owner.must("owner", "status", "--channel", "checkout")
	lines := strings.Split(strings.TrimSuffix(first.stderr, "\n"), "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[0], "crosshall: round limit reached in #checkout for @form's new threads at ") ||
		lines[1] != "next: crosshall owner status --channel checkout shows it. crosshall owner budget grant checkout/@form --rounds 8 lets @form open threads again." ||
		first.obj["ok"] != true {
		t.Fatalf("first owner command: stderr %q stdout %q", first.stderr, first.stdout)
	}
	if second := owner.must("owner", "status", "--channel", "checkout"); second.stderr != "" {
		t.Fatalf("the stop was printed again: %q", second.stderr)
	}
}

// Setup's own owner requests print no owner line, so a stop it passes over
// stays for the next owner command, with desktop notifications off.
func TestSetupLeavesAStopForTheNextOwnerCommand(t *testing.T) {
	f := newSetupWorld(t)
	owner := f.owner()
	owner.extra = []string{"CROSSHALL_NOTIFY=off"}
	owner.must("owner", "start")
	owner.must("owner", "channel", "create", "checkout")
	form, tax := f.member(), f.member()
	form.mustFixtureWrite("join", "checkout", "--as", "form")
	tax.mustFixtureWrite("join", "checkout", "--as", "tax")
	owner.must("owner", "budget", "grant", "checkout/@form", "--rounds", "0")
	if o := form.run("ask", "@tax", "--new", "stopped"); o.exit != 3 || o.obj["code"] != "round_limit" {
		t.Fatalf("refusal: exit %d %v", o.exit, o.obj)
	}
	if applied := f.apply("", "setup", "claude", "--apply"); strings.Contains(applied.stderr, "round limit") {
		t.Fatalf("setup printed the stop: %q", applied.stderr)
	}
	if status := owner.must("owner", "status", "--channel", "checkout"); !strings.HasPrefix(status.stderr,
		"crosshall: round limit reached in #checkout for @form's new threads at ") {
		t.Fatalf("the owner command after setup: stderr %q", status.stderr)
	}
}
