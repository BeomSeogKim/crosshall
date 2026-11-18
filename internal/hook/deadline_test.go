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

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func TestHookDeadlineWithSlowCounterOpen(t *testing.T) {
	_, dir, _ := runFixture(t)
	synctest.Test(t, func(t *testing.T) {
		entry := time.Now()
		reader, writer := io.Pipe()
		defer writer.Close()
		defer reader.Close()
		var output bytes.Buffer
		rv, err := os.OpenRoot(dir)
		if err != nil {
			t.Fatal(err)
		}
		prepare := func() (*os.Root, *hookCounter) {
			time.Sleep(30 * time.Millisecond) // virtual filesystem latency
			return rv, openHookCounter(rv, "codex", "UserPromptSubmit", entry)
		}
		runTimed(entry, prepare, func(ctx context.Context, rv *os.Root) string {
			return dispatchEvent(ctx, entry, rv, recordedTransport{}, []string{"codex", "UserPromptSubmit"}, reader, &output, nil)
		})
		if elapsed := time.Since(entry); elapsed > Budget || output.Len() != 0 {
			t.Fatalf("open stdin returned after %v with %q", elapsed, output.String())
		}
		raw, err := os.ReadFile(filepath.Join(dir, "hooks", entry.UTC().Format("20060102")))
		if err != nil || string(raw) != "codex UserPromptSubmit deadline 145\n" {
			t.Fatalf("counter after delayed open: %q, %v", raw, err)
		}
	})
}

type counterWrite func([]byte) (int, error)

func (f counterWrite) Write(p []byte) (int, error) { return f(p) }
func (counterWrite) Close() error                  { return nil }

func TestHookCounterAfterSuspension(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		entry := time.Now()
		var rows bytes.Buffer
		counter := &hookCounter{file: counterWrite(rows.Write), prefix: "codex Stop "}
		// Resume at the accounting boundary with both timers already overdue.
		time.Sleep(200 * time.Millisecond)
		finishCounter(entry, "rpc", counter)
		if got := rows.String(); got != "codex Stop deadline 200\n" {
			t.Fatalf("overdue outcome: %q", got)
		}
		// A late invocation cannot start a lookup for an unopened counter.
		Run(entry, []string{"codex", "Stop"}, nil, nil, func() (string, error) {
			t.Error("started a new rendezvous lookup after the deadline")
			return "", nil
		})
	})
}

func TestStalledCounterAppendIsBounded(t *testing.T) {
	for _, tc := range []struct {
		name     string
		at, want time.Duration
	}{
		{"before deadline", Budget - 2*time.Millisecond, 2 * time.Millisecond},
		{"after suspension", 2 * Budget, counterAllowance},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				release := make(chan struct{})
				defer close(release)
				attempts := 0
				counter := &hookCounter{file: counterWrite(func(p []byte) (int, error) {
					attempts++
					<-release
					return len(p), nil
				})}
				entry := time.Now()
				time.Sleep(tc.at)
				start := time.Now()
				finishCounter(entry, "deadline", counter)
				synctest.Wait()
				if waited := time.Since(start); waited != tc.want || attempts != 1 {
					t.Fatalf("waited %v for %d append attempts", waited, attempts)
				}
			})
		})
	}
}

func TestStalledPreparationDoesNotGetAccountingAllowance(t *testing.T) {
	rv, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	synctest.Test(t, func(t *testing.T) {
		entry := time.Now()
		release := make(chan struct{})
		var rows bytes.Buffer
		runTimed(entry, func() (*os.Root, *hookCounter) {
			<-release
			return rv, &hookCounter{file: counterWrite(rows.Write)}
		}, func(context.Context, *os.Root) string {
			t.Error("dispatched after the preparation deadline")
			return "rpc"
		})
		elapsed := time.Since(entry)
		close(release)
		synctest.Wait()
		if elapsed != Budget || rows.Len() != 0 {
			t.Fatalf("stalled preparation took %v with rows %q", elapsed, rows.String())
		}
	})
}

// Model a late timer notification independently of the monotonic clock.
type undeliveredDeadline struct {
	context.Context
	end time.Time
}

func (d undeliveredDeadline) Deadline() (time.Time, bool) { return d.end, true }

func TestLateHookReplyProducesNoVendorOutput(t *testing.T) {
	rv, _, _ := runFixture(t)
	synctest.Test(t, func(t *testing.T) {
		entry := time.Now()
		ctx := undeliveredDeadline{t.Context(), entry.Add(Budget - counterAllowance)}
		calls := 0
		transport := recordedTransport{call: func(ident.Identity, string, wire.Request, bool) (spool.Reply, error) {
			calls++
			time.Sleep(2 * Budget)
			return spool.Reply{Response: wire.Response{Result: core.Result{
				Data: json.RawMessage(`{"epoch":7,"pending_line":"[crosshall] pending: 42"}`),
			}}}, nil
		}}
		var out bytes.Buffer
		input := strings.NewReader(`{"hook_event_name":"UserPromptSubmit","session_id":"fixture-thread","turn_id":"turn"}`)
		outcome := dispatchEvent(ctx, entry, rv, transport, []string{"codex", "UserPromptSubmit"}, input, &out, func(string) (string, bool) { return "", false })
		if calls != 1 || outcome != "deadline" || out.Len() != 0 {
			t.Fatalf("late reply made %d calls, returned %s and emitted %q", calls, outcome, out.String())
		}
	})
}
