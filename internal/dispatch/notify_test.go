package dispatch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/store"
)

func notifyStore(t *testing.T) *store.Store {
	t.Helper()
	root := filepath.Join(t.TempDir(), "state")
	if err := os.Mkdir(root, 0700); err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// addMemberStop records an open member stop the way the domain does.
func addMemberStop(t *testing.T, db *store.Store, principal int64, sender string, at time.Time) {
	t.Helper()
	err := db.Update(context.Background(), func(tx store.Tx) error {
		_, err := tx.ExecContext(context.Background(), fmt.Sprintf(`
			INSERT INTO principal (id, kind, vendor, created_at_ms, guard_told) VALUES (%[1]d, 'session', 'codex', 1, 1);
			INSERT INTO event (kind, subject, detail, at_ms) VALUES ('loop_guard_stopped', 'principal:%[1]d',
				'{"channel":"dev","sender":"%[2]s","recipients":["b"],"round":9,"round_limit":8}', %[3]d);
			INSERT INTO owner_alert (event_seq) VALUES (last_insert_rowid())`, principal, sender, at.UnixMilli()))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func alertStates(t *testing.T, db *store.Store) []string {
	t.Helper()
	var out []string
	err := db.Update(context.Background(), func(tx store.Tx) error {
		rows, err := tx.QueryContext(context.Background(), `SELECT desktop_state FROM owner_alert ORDER BY event_seq`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var state string
			if err := rows.Scan(&state); err != nil {
				return err
			}
			out = append(out, state)
		}
		return rows.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// fakeNotify writes a stand-in for the notification command that records
// its arguments, one per line, then runs then.
func fakeNotify(t *testing.T, then string) (path, args string) {
	t.Helper()
	dir := t.TempDir()
	path, args = filepath.Join(dir, "notify"), filepath.Join(dir, "args")
	script := fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$@\" > '%s'\n%s\n", args, then)
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return path, args
}

func recordedArgs(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
}

const release = "v0.2.0"

func TestNotifierShowsAStopOnceWithTitleAndBodyAsArguments(t *testing.T) {
	db := notifyStore(t)
	now := time.Now()
	addMemberStop(t, db, 1, "a", now)
	command, args := fakeNotify(t, "exit 0")
	_, darwin := notifyCommand("darwin")
	n := newNotifier(db, release, false, command, darwin)
	n.tick(t.Context(), now)
	n.wait()
	want := []string{"-e", "on run argv", "-e", "display notification (item 1 of argv) with title (item 2 of argv)", "-e", "end run",
		"@a's new threads stopped. @a's request to @b would be round 9 of 8. Run: crosshall owner status", "crosshall: round limit in #dev"}
	if got := recordedArgs(t, args); !reflect.DeepEqual(got, want) {
		t.Fatalf("arguments %q, want %q", got, want)
	}
	if got := alertStates(t, db); !reflect.DeepEqual(got, []string{"sent"}) {
		t.Fatalf("states %v", got)
	}
	os.Remove(args)
	n.tick(t.Context(), now.Add(2*time.Minute))
	n.wait()
	if got := recordedArgs(t, args); got != nil {
		t.Fatalf("a sent stop was shown again: %q", got)
	}
}

func TestNotifyCommandKeepsTextOutOfTheScript(t *testing.T) {
	title, body := `crosshall: round limit in #dev" & (do shell script "touch /tmp/x") & "`, `-e x`
	path, darwin := notifyCommand("darwin")
	got := darwin(title, body)
	if path != "/usr/bin/osascript" || !reflect.DeepEqual(got[:6], []string{"-e", "on run argv", "-e",
		"display notification (item 1 of argv) with title (item 2 of argv)", "-e", "end run"}) ||
		!reflect.DeepEqual(got[6:], []string{body, title}) {
		t.Fatalf("darwin %s %q", path, got)
	}
	path, linux := notifyCommand("linux")
	if got := linux(title, body); path != "/usr/bin/notify-send" || !reflect.DeepEqual(got, []string{"--app-name=crosshall", title, body}) {
		t.Fatalf("linux %s %q", path, got)
	}
	if path, _ := notifyCommand("freebsd"); path != "" {
		t.Fatalf("freebsd has a notification command: %s", path)
	}
}

func TestNotifierRecordsAFailingOrHangingCommandAsFailed(t *testing.T) {
	if notifyTimeout != 5*time.Second {
		t.Fatalf("timeout %v, want 5s", notifyTimeout)
	}
	for _, then := range []string{"exit 1", "exec sleep 30"} {
		t.Run(then, func(t *testing.T) {
			db := notifyStore(t)
			now := time.Now()
			addMemberStop(t, db, 1, "a", now)
			command, _ := fakeNotify(t, then)
			_, args := notifyCommand("linux")
			n := newNotifier(db, release, false, command, args)
			n.timeout = 200 * time.Millisecond
			start := time.Now()
			n.tick(t.Context(), now)
			n.wait()
			if elapsed := time.Since(start); elapsed > 3*time.Second {
				t.Fatalf("the command ran for %v", elapsed)
			}
			if got := alertStates(t, db); !reflect.DeepEqual(got, []string{"failed"}) {
				t.Fatalf("states %v", got)
			}
		})
	}
}

func TestNotifierIsUnavailableOutsideReleasesWhenOffOrWithoutACommand(t *testing.T) {
	for name, tc := range map[string]struct {
		version string
		off     bool
		missing bool
	}{
		"development build": {version: "dev"},
		"CROSSHALL_NOTIFY":  {version: release, off: true},
		"no command":        {version: release, missing: true},
	} {
		t.Run(name, func(t *testing.T) {
			db := notifyStore(t)
			now := time.Now()
			addMemberStop(t, db, 1, "a", now)
			command, args := fakeNotify(t, "exit 0")
			if tc.missing {
				command = filepath.Join(t.TempDir(), "missing")
			}
			_, linux := notifyCommand("linux")
			n := newNotifier(db, tc.version, tc.off, command, linux)
			n.tick(t.Context(), now)
			n.wait()
			if got := alertStates(t, db); !reflect.DeepEqual(got, []string{"unavailable"}) || recordedArgs(t, args) != nil {
				t.Fatalf("states %v, command ran: %t", got, recordedArgs(t, args) != nil)
			}
		})
	}
}

func TestNotifyOffReadsTheEnvironment(t *testing.T) {
	for value, want := range map[string]bool{"off": true, " OFF ": true, "": false, "on": false} {
		t.Setenv(notifyEnv, value)
		if got := notifyOff(); got != want {
			t.Errorf("%s=%q: off %t, want %t", notifyEnv, value, got, want)
		}
	}
}

func TestDispatcherEnvForwardsTheNotifySwitch(t *testing.T) {
	t.Setenv(notifyEnv, "off")
	if got := (LaunchConfig{StateRoot: t.TempDir()}).dispatcherEnv(); !slices.Contains(got, notifyEnv+"=off") {
		t.Fatalf("dispatcher child environment lacks %s: %v", notifyEnv, got)
	}
}

// notify-send reaches the desktop through the session bus, which the
// dispatcher, and so the command it runs, must inherit.
func TestDispatcherEnvForwardsTheDesktopSession(t *testing.T) {
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "unix:path=/run/user/1000/bus")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	got := (LaunchConfig{StateRoot: t.TempDir()}).dispatcherEnv()
	for _, want := range []string{"DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/1000/bus", "XDG_RUNTIME_DIR=/run/user/1000"} {
		if !slices.Contains(got, want) {
			t.Errorf("dispatcher child environment lacks %s: %v", want, got)
		}
	}
}

// The command runs outside every transaction and outside the dispatcher
// loop; one runs at a time, and one cut off by a stop is not shown again.
func TestNotifierHoldsNeitherTheLoopNorTheStore(t *testing.T) {
	db := notifyStore(t)
	now := time.Now()
	addMemberStop(t, db, 1, "a", now)
	dir := t.TempDir()
	started := filepath.Join(dir, "started")
	command, args := fakeNotify(t, fmt.Sprintf("touch '%s'\nexec sleep 30", started))
	_, linux := notifyCommand("linux")
	n := newNotifier(db, release, false, command, linux)
	n.timeout = time.Minute
	ctx, cancel := context.WithCancel(t.Context())
	begin := time.Now()
	n.tick(ctx, now)
	if elapsed := time.Since(begin); elapsed > time.Second {
		t.Fatalf("tick waited %v for the command", elapsed)
	}
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if _, err := os.Stat(started); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the command did not start")
		}
	}
	addMemberStop(t, db, 2, "c", now.Add(time.Minute))
	n.tick(ctx, now.Add(2*time.Minute))
	if got := alertStates(t, db); !reflect.DeepEqual(got, []string{"sending", "pending"}) {
		t.Fatalf("while a command runs: states %v", got)
	}
	cancel()
	n.wait()

	restarted := newNotifier(db, release, false, command, linux)
	if err := restarted.recover(t.Context()); err != nil {
		t.Fatal(err)
	}
	os.Remove(args)
	restarted.timeout = 200 * time.Millisecond
	restarted.tick(t.Context(), now.Add(2*time.Minute))
	restarted.wait()
	if got := alertStates(t, db); !reflect.DeepEqual(got, []string{"failed", "failed"}) {
		t.Fatalf("after the restart: states %v", got)
	}
	if got := recordedArgs(t, args); len(got) != 3 || !strings.HasPrefix(got[2], "@c's") {
		t.Fatalf("after the restart the command got %q, want only the second stop", got)
	}
}
