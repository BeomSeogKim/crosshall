package hook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
		wrote := runTimed(entry, StartBudget, StartBudget, &output, prepare, func(ctx context.Context, rv *os.Root, w io.Writer) string {
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

// A start whose preparation took longer than the ordinary budget still
// records its outcome: its counter opens within its own budget.
func TestASlowStartStillOpensItsCounter(t *testing.T) {
	rv, _, _ := runFixture(t)
	entry := time.Now().Add(-200 * time.Millisecond)
	counter := openHookCounter(rv, "codex", "SessionStart", entry)
	if counter == nil {
		t.Fatal("no counter for a start 200 ms after entry")
	}
	counter.file.Close()
	if c := openHookCounter(rv, "codex", "Stop", entry); c != nil {
		c.file.Close()
		t.Fatal("a Stop 200 ms after entry opened a counter")
	}
}

// ctxTransport answers a first contact after join and an event after
// event, unless the callback's deadline comes first; the first contact
// restores the parked leaf.
type ctxTransport struct {
	join, event  time.Duration
	parked, leaf string
	events       *int
}

func wait(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c ctxTransport) HookLookup(ctx context.Context, _ ident.Identity, intent string) (string, wire.Response, error) {
	if err := wait(ctx, c.join); err != nil {
		return "", wire.Response{}, err
	}
	if intent != "join" {
		return "", wire.Response{}, fmt.Errorf("intent %q", intent)
	}
	if err := os.Rename(c.parked, c.leaf); err != nil {
		return "", wire.Response{}, err
	}
	return "fixture-gen", wire.Response{Result: core.Result{Status: core.ExitOK}}, nil
}

func (c ctxTransport) HookCall(ctx context.Context, _ ident.Identity, _ string, req wire.Request, _ bool) (spool.Reply, error) {
	if req.Verb != wire.VerbHookEvent {
		return spool.Reply{}, nil
	}
	if err := wait(ctx, c.event); err != nil {
		return spool.Reply{}, err
	}
	*c.events++
	return spool.Reply{Response: wire.Response{Result: core.Result{Status: core.ExitOK, Data: json.RawMessage(`{"epoch":7}`)}}}, nil
}

// A prompt that recovers a missed start (no leaf) or repairs one (its
// session started in a repository it maps) keeps the start budget: its
// event still goes out when the join or the event itself took 200 ms.
func TestAPromptThatRecoversOrRepairsAStartKeepsTheStartBudget(t *testing.T) {
	for _, recovering := range []bool{true, false} {
		t.Run(fmt.Sprintf("recovering %t", recovering), func(t *testing.T) {
			rv, dir, leaf := runFixture(t)
			repo, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(repo, ".git"), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "repo-defaults"), []byte(repoDigestRecord(repo)), 0600); err != nil {
				t.Fatal(err)
			}
			// The session's start was in that repository.
			state := t.TempDir()
			records := filepath.Join(state, startupRepoDir)
			if err := os.Chmod(state, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(records, 0700); err != nil {
				t.Fatal(err)
			}
			id := ident.Identity{Vendor: "codex", Key: "fixture-thread"}
			if err := os.WriteFile(filepath.Join(records, id.Digest()), []byte(repoDigestRecord(repo)), 0600); err != nil {
				t.Fatal(err)
			}
			parked := filepath.Join(dir, "parked")
			if recovering {
				if err := os.Rename(leaf, parked); err != nil {
					t.Fatal(err)
				}
			}
			events := 0
			transport := ctxTransport{event: 200 * time.Millisecond, parked: parked, leaf: leaf, events: &events}
			if recovering {
				transport = ctxTransport{join: 200 * time.Millisecond, parked: parked, leaf: leaf, events: &events}
			}
			stdin := fmt.Sprintf(`{"session_id":"fixture-thread","hook_event_name":"UserPromptSubmit","turn_id":"t1","prompt":"p","cwd":%q}`, repo)
			synctest.Test(t, func(t *testing.T) {
				entry := time.Now()
				var out strings.Builder
				prepare := func() (*os.Root, *hookCounter) {
					return rv, openHookCounter(rv, "codex", "UserPromptSubmit", entry)
				}
				runTimed(entry, Budget, StartBudget, &out, prepare, func(ctx context.Context, rv *os.Root, w io.Writer) string {
					return dispatchCallback(ctx, entry, rv, transport, []string{"codex", "UserPromptSubmit"}, strings.NewReader(stdin), w,
						func(string) (string, bool) { return "", false }, callbackState{root: state})
				})
				time.Sleep(time.Second) // let a dispatch the deadline left behind finish
				raw, err := os.ReadFile(filepath.Join(dir, "hooks", entry.UTC().Format("20060102")))
				if err != nil || string(raw) != "codex UserPromptSubmit rpc 200\n" || events != 1 {
					t.Fatalf("counter %q (%v) with %d events", raw, err, events)
				}
			})
		})
	}
}

// A prompt starts on the ordinary budget: one that stalls before it knows
// it enrolls anything (stdin, its leaf) gives up at that deadline. One
// that extends its budget, because it recovers or repairs its session,
// keeps the start budget.
func TestAPromptRunsOnTheOrdinaryBudgetUntilItExtends(t *testing.T) {
	for _, extends := range []bool{false, true} {
		t.Run(fmt.Sprintf("extends %t", extends), func(t *testing.T) {
			_, dir, _ := runFixture(t)
			synctest.Test(t, func(t *testing.T) {
				entry := time.Now()
				rv, err := os.OpenRoot(dir)
				if err != nil {
					t.Fatal(err)
				}
				var output bytes.Buffer
				prepare := func() (*os.Root, *hookCounter) {
					return rv, openHookCounter(rv, "codex", "UserPromptSubmit", entry)
				}
				runTimed(entry, Budget, StartBudget, &output, prepare, func(ctx context.Context, rv *os.Root, w io.Writer) string {
					if extends {
						extendBudget(ctx)
					}
					time.Sleep(250 * time.Millisecond) // a stall, or a slow join
					if hookExpired(ctx) {
						return "deadline"
					}
					return "rpc"
				})
				took := time.Since(entry)
				time.Sleep(time.Second) // let a dispatch the deadline left behind finish
				raw, err := os.ReadFile(filepath.Join(dir, "hooks", entry.UTC().Format("20060102")))
				want, within := "codex UserPromptSubmit deadline 145\n", Budget
				if extends {
					want, within = "codex UserPromptSubmit rpc 250\n", StartBudget
				}
				if err != nil || string(raw) != want || took > within {
					t.Fatalf("returned after %v; counter %q, %v; want %q within %v", took, raw, err, want, within)
				}
			})
		})
	}
}

// A prompt starts on the ordinary budget and may extend to the start
// budget; a start has the start budget from its entry.
func TestAPromptStartsOnTheOrdinaryBudget(t *testing.T) {
	for _, tc := range []struct {
		adapter, event string
		want           time.Duration
	}{
		{"claude", "SessionStart", StartBudget},
		{"codex", "SessionStart", StartBudget},
		{"claude", "UserPromptSubmit", Budget},
		{"codex", "UserPromptSubmit", Budget},
		{"claude", "Stop", Budget},
		{"nosuchadapter", "UserPromptSubmit", Budget},
	} {
		if got := CallbackBase(tc.adapter, tc.event); got != tc.want {
			t.Errorf("%s %s: %v, want %v", tc.adapter, tc.event, got, tc.want)
		}
	}
}

// A prompt whose stdin never closes gives up at the ordinary deadline:
// until it knows it recovers or repairs, it has no more time.
func TestAPromptWithStalledStdinReturnsWithinTheOrdinaryBudget(t *testing.T) {
	_, dir, _ := runFixture(t)
	state := t.TempDir()
	if err := os.Chmod(state, 0700); err != nil {
		t.Fatal(err)
	}
	locate := func() (Paths, error) { return Paths{State: state, Rendezvous: dir}, nil }
	synctest.Test(t, func(t *testing.T) {
		stdin, writer := io.Pipe()
		defer writer.Close()
		var out bytes.Buffer
		began := time.Now()
		Run(began, []string{"codex", "UserPromptSubmit"}, stdin, &out, locate)
		if took := time.Since(began); took > Budget || out.Len() != 0 {
			t.Fatalf("returned after %v with %q", took, out.String())
		}
	})
}

// A prompt that did not extend its budget gives its stalled counter
// record only what is left of the ordinary budget.
func TestAStalledCounterRecordKeepsAPromptOnTheOrdinaryBudget(t *testing.T) {
	rv, _, _ := runFixture(t)
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		defer close(release)
		counter := &hookCounter{prefix: "codex UserPromptSubmit ", file: counterWrite(func(p []byte) (int, error) {
			<-release
			return len(p), nil
		})}
		entry := time.Now()
		runTimed(entry, Budget, StartBudget, io.Discard, func() (*os.Root, *hookCounter) { return rv, counter },
			func(context.Context, *os.Root, io.Writer) string { return "clean" })
		if took := time.Since(entry); took > Budget {
			t.Fatalf("returned after %v", took)
		}
	})
}
