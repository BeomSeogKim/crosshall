package hook

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// With the rendezvous gone, as after a reboot, the state root's policy copy
// decides whether a hook starts the dispatcher. A start that fails is
// written to dispatcher.log and the hook returns at once, printing nothing.
func TestAHookStartsOnlyForAnEnabledAdapterAndLogsAFailure(t *testing.T) {
	for name, c := range map[string]struct {
		policy string // the state root's copy; "" for none
		starts int32
	}{
		"enabled":              {"claude 1 0 3\ncodex 0 0 1\n", 1},
		"off":                  {"claude 0 0 3\n", 0},
		"never published":      {"", 0},
		"another adapter only": {"codex 1 0 1\n", 0},
	} {
		state := t.TempDir()
		if err := os.Chmod(state, 0700); err != nil { // as owner start creates it
			t.Fatal(err)
		}
		if c.policy != "" {
			if err := os.WriteFile(filepath.Join(state, StatePolicyName), []byte(c.policy), 0600); err != nil {
				t.Fatal(err)
			}
		}
		var starts atomic.Int32
		locate := func() (Paths, error) {
			return Paths{State: state, Rendezvous: filepath.Join(state, "rendezvous-cleared-at-boot"),
				Start: func(context.Context) (bool, error) {
					starts.Add(1)
					return false, errors.New("fixture start failure")
				}}, nil
		}
		var out bytes.Buffer
		began := time.Now()
		Run(began, []string{"claude", "SessionStart"}, strings.NewReader(`{"hook_event_name":"SessionStart","session_id":"s","source":"startup"}`), &out, locate)
		if took := time.Since(began); took >= Budget {
			t.Errorf("%s: a failed start held the hook for %v", name, took)
		}
		if out.Len() != 0 || starts.Load() != c.starts {
			t.Errorf("%s: printed %q, %d starts", name, out.String(), starts.Load())
		}
		log, _ := os.ReadFile(filepath.Join(state, "dispatcher.log"))
		failed := strings.Contains(string(log), "hook claude SessionStart: could not start the dispatcher: fixture start failure (")
		if failed != (c.starts == 1) {
			t.Errorf("%s: dispatcher.log %q", name, log)
		}
	}
}

// A launched dispatcher that is not ready by the start deadline leaves the
// session start clean and inside the vendor timeout, and says so in the log.
func TestASessionStartWaitsOnlyUntilTheStartDeadline(t *testing.T) {
	state := t.TempDir()
	if err := os.Chmod(state, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(state, StatePolicyName), []byte("claude 1 0 3\n"), 0600); err != nil {
		t.Fatal(err)
	}
	locate := func() (Paths, error) {
		return Paths{State: state, Rendezvous: filepath.Join(state, "rendezvous-cleared-at-boot"),
			Start: func(ctx context.Context) (bool, error) {
				<-ctx.Done()
				return true, fmt.Errorf("dispatcher did not become ready: %w", ctx.Err())
			}}, nil
	}
	var out bytes.Buffer
	began := time.Now()
	Run(began, []string{"claude", "SessionStart"}, strings.NewReader(`{"hook_event_name":"SessionStart","session_id":"s","source":"startup"}`), &out, locate)
	if took := time.Since(began); took < startWait || took >= startDeadline+logAllowance+50*time.Millisecond {
		t.Fatalf("the session start returned after %v", took)
	}
	log, _ := os.ReadFile(filepath.Join(state, "dispatcher.log"))
	if out.Len() != 0 || !strings.Contains(string(log), "hook claude SessionStart: start_pending: ") {
		t.Fatalf("printed %q, dispatcher.log %q", out.String(), log)
	}
}
