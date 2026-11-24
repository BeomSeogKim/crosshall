package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// A callback that may enroll its session gets the start budget: a start,
// and a prompt, which enrolls a session whose start was missed. Every
// other callback keeps the ordinary budget.
func TestCallbacksThatMayEnrollGetTheStartBudget(t *testing.T) {
	for _, tc := range []struct {
		adapter, event string
		want           time.Duration
	}{
		{"claude", "SessionStart", StartBudget},
		{"codex", "SessionStart", StartBudget},
		{"agy", "SessionStart", StartBudget},
		{"claude", "UserPromptSubmit", StartBudget},
		{"codex", "UserPromptSubmit", StartBudget},
		{"claude", "Stop", Budget},
		{"codex", "PermissionRequest", Budget},
		{"claude", "NoSuchEvent", Budget},
		{"nosuchadapter", "SessionStart", Budget},
	} {
		if got := CallbackBudget(tc.adapter, tc.event); got != tc.want {
			t.Errorf("%s %s: %v, want %v", tc.adapter, tc.event, got, tc.want)
		}
	}
}

// A start answered after the ordinary budget but inside its own is
// answered, and its counter records it as such.
func TestAStartAnsweredInsideTheStartBudgetIsNotADeadline(t *testing.T) {
	_, dir, _ := runFixture(t)
	synctest.Test(t, func(t *testing.T) {
		entry := time.Now()
		rv, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		var output bytes.Buffer
		prepare := func() (*os.Root, *hookCounter) {
			return rv, openHookCounter(rv, "codex", "SessionStart", entry)
		}
		wrote := runTimed(entry, StartBudget, &output, prepare, func(ctx context.Context, rv *os.Root, w io.Writer) string {
			time.Sleep(250 * time.Millisecond) // a dispatcher slowed by disk flushes
			if hookExpired(ctx) {
				return "deadline"
			}
			_, _ = io.WriteString(w, "answered\n")
			return "rpc"
		})
		time.Sleep(time.Second) // let a dispatch the deadline left behind finish
		if !wrote || output.String() != "answered\n" {
			t.Fatalf("wrote %t, output %q", wrote, output.String())
		}
		raw, err := os.ReadFile(filepath.Join(dir, "hooks", entry.UTC().Format("20060102")))
		if err != nil || string(raw) != "codex SessionStart rpc 250\n" {
			t.Fatalf("counter: %q, %v", raw, err)
		}
	})
}

type slowTransport struct{ delay time.Duration }

func (slowTransport) HookLookup(context.Context, ident.Identity, string) (string, wire.Response, error) {
	return "", wire.Response{}, os.ErrNotExist
}

func (s slowTransport) HookCall(ctx context.Context, _ ident.Identity, _ string, _ wire.Request, _ bool) (spool.Reply, error) {
	select {
	case <-time.After(s.delay):
		return spool.Reply{Response: wire.Response{Data: json.RawMessage(`{"epoch":7}`)}}, nil
	case <-ctx.Done():
		return spool.Reply{}, ctx.Err()
	}
}

// A prompt of an enrolled session that repairs nothing keeps the ordinary
// budget, even though prompts start with the start budget.
func TestAPromptThatEnrollsNothingKeepsTheOrdinaryBudget(t *testing.T) {
	rv, _, _ := runFixture(t)
	synctest.Test(t, func(t *testing.T) {
		entry := time.Now()
		ctx, cancel := context.WithDeadline(t.Context(), entry.Add(StartBudget))
		defer cancel()
		stdin := `{"session_id":"fixture-thread","hook_event_name":"UserPromptSubmit","turn_id":"t1","prompt":"p"}`
		var out strings.Builder
		outcome := dispatchEvent(ctx, entry, rv, slowTransport{delay: 200 * time.Millisecond}, []string{"codex", "UserPromptSubmit"},
			strings.NewReader(stdin), &out, func(string) (string, bool) { return "", false })
		if outcome != "deadline" || time.Since(entry) > Budget {
			t.Fatalf("outcome %q after %v, want deadline within %v", outcome, time.Since(entry), Budget)
		}
	})
}

// A callback that waited for the dispatcher to start keeps what is left of
// the start deadline, never more than its own budget.
func TestACallbackThatStartedTheDispatcherKeepsTheRestOfTheStartDeadline(t *testing.T) {
	began := time.Now()
	for _, tc := range []struct {
		started time.Duration
		want    time.Duration
	}{
		{50 * time.Millisecond, StartBudget},
		{startWait, Budget},
		{500 * time.Millisecond, startDeadline - 2*logAllowance - 500*time.Millisecond},
	} {
		if got := afterStart(began, began.Add(tc.started), StartBudget); got != tc.want {
			t.Errorf("started after %v: %v, want %v", tc.started, got, tc.want)
		}
	}
}
