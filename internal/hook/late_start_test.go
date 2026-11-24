package hook

import (
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

	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/proc"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// A prompt of a verified process launch carries the launch's record time
// and the hook's PID namespace, which the dispatcher needs to bind a launch
// whose start it never saw; no other callback sends them.
func TestOnlyAPromptOfAProcessLaunchCarriesTheLateStartProof(t *testing.T) {
	for _, under := range []bool{true, false} {
		t.Run(fmt.Sprintf("hosted-%t", under), func(t *testing.T) {
			hosted(t, under)
			rv, dir, oldLeaf := runFixture(t)
			id := ident.Identity{Vendor: "claude", Key: "fixture-thread"}
			if err := os.Rename(oldLeaf, filepath.Join(dir, "m", id.Digest()[:16])); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "policy"), []byte("claude 1 0 7\n"), 0600); err != nil {
				t.Fatal(err)
			}
			var sent []wire.HookEventArgs
			transport := recordedTransport{call: func(_ ident.Identity, _ string, req wire.Request, _ bool) (spool.Reply, error) {
				if req.Verb == wire.VerbHookEvent {
					var a wire.HookEventArgs
					if err := json.Unmarshal(req.Args, &a); err != nil {
						t.Fatal(err)
					}
					sent = append(sent, a)
				}
				return spool.Reply{}, nil
			}}
			lookup := func(key string) (string, bool) {
				switch key {
				case "CLAUDE_PID":
					return "72", true
				case "CLAUDE_CODE_SESSION_ID":
					return "fixture-thread", true
				}
				return "", false
			}
			for index, name := range []string{"UserPromptSubmit", "Stop", "UserPromptSubmit"} {
				input := fmt.Sprintf(`{"hook_event_name":%q,"session_id":"fixture-thread","prompt_id":"turn-%d"}`, name, index)
				if name == "Stop" {
					input = `{"hook_event_name":"Stop","session_id":"fixture-thread","prompt_id":"turn-0","stop_hook_active":false}`
				}
				var out strings.Builder
				dispatchEvent(t.Context(), time.UnixMilli(200+int64(index)), rv, transport, []string{"claude", name}, strings.NewReader(input), &out, lookup)
			}
			// A late start marks the process as one a member's session started
			// in, as a start does, so a later /clear there is asked about.
			if launchMarked(rv, "72") != under {
				t.Fatalf("process marked %t, want %t", launchMarked(rv, "72"), under)
			}
			if len(sent) != 3 {
				t.Fatalf("%d events sent, want 3", len(sent))
			}
			for i, a := range sent {
				prompt := a.Fact == wire.HookPromptSubmitted
				switch {
				case !under:
					if a.LaunchVerified || a.LaunchAtMS != 0 || a.PIDNamespace != "" {
						t.Fatalf("event %d outside the process: %+v", i, a)
					}
				case !a.LaunchVerified || a.LaunchRef != sent[0].LaunchRef:
					t.Fatalf("event %d reports launch %q (verified %t), want %q", i, a.LaunchRef, a.LaunchVerified, sent[0].LaunchRef)
				case prompt && (a.LaunchAtMS != 200 || a.PIDNamespace != proc.PIDNamespace()):
					t.Fatalf("prompt %d: launch_at_ms %d, pid_namespace %q", i, a.LaunchAtMS, a.PIDNamespace)
				case !prompt && (a.LaunchAtMS != 0 || a.PIDNamespace != ""):
					t.Fatalf("a %s carries the late-start proof: %+v", a.Fact, a)
				}
			}
		})
	}
}

// A prompt that binds a launch whose start was missed keeps the start
// budget: the dispatcher binds the launch in the prompt's request. A prompt
// of a session nested under the process binds nothing and keeps the
// ordinary one.
func TestAPromptThatBindsAMissedStartKeepsTheStartBudget(t *testing.T) {
	for _, under := range []bool{true, false} {
		t.Run(fmt.Sprintf("hosted-%t", under), func(t *testing.T) {
			hosted(t, under)
			rv, dir, oldLeaf := runFixture(t)
			id := ident.Identity{Vendor: "claude", Key: "fixture-thread"}
			if err := os.Rename(oldLeaf, filepath.Join(dir, "m", id.Digest()[:16])); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "policy"), []byte("claude 1 0 7\n"), 0600); err != nil {
				t.Fatal(err)
			}
			lookup := func(key string) (string, bool) {
				switch key {
				case "CLAUDE_PID":
					return "72", true
				case "CLAUDE_CODE_SESSION_ID":
					return "fixture-thread", true
				}
				return "", false
			}
			events := 0
			transport := ctxTransport{event: 200 * time.Millisecond, events: &events}
			stdin := `{"hook_event_name":"UserPromptSubmit","session_id":"fixture-thread","prompt_id":"turn-0"}`
			synctest.Test(t, func(t *testing.T) {
				entry := time.Now()
				var out strings.Builder
				prepare := func() (*os.Root, *hookCounter) {
					return rv, openHookCounter(rv, "claude", "UserPromptSubmit", entry)
				}
				runTimed(entry, Budget, StartBudget, &out, prepare, func(ctx context.Context, rv *os.Root, w io.Writer) string {
					return dispatchCallback(ctx, entry, rv, transport, []string{"claude", "UserPromptSubmit"}, strings.NewReader(stdin), w,
						lookup, callbackState{root: t.TempDir()})
				})
				time.Sleep(time.Second) // let a dispatch the deadline left behind finish
				raw, err := os.ReadFile(filepath.Join(dir, "hooks", entry.UTC().Format("20060102")))
				want, sent := "claude UserPromptSubmit rpc 200\n", 1
				if !under {
					want, sent = "claude UserPromptSubmit deadline 145\n", 0
				}
				if err != nil || string(raw) != want || events != sent {
					t.Fatalf("counter %q (%v) with %d events", raw, err, events)
				}
			})
		})
	}
}
