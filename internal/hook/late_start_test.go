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

// A prompt, turn end or idle prompt of a verified process launch carries
// the launch's record time and the hook's PID namespace, which the
// dispatcher needs to bind a launch whose start it never saw; the first of
// them binds the launch, also a turn end of the turn a hand join ran in
// (#437). No other callback sends them, and none sends them to a dispatcher
// that does not offer to accept them.
func TestATurnOfAProcessLaunchCarriesTheLateStartProof(t *testing.T) {
	for _, c := range []struct{ under, offered bool }{{true, true}, {false, true}, {true, false}} {
		t.Run(fmt.Sprintf("hosted-%t-offered-%t", c.under, c.offered), func(t *testing.T) {
			under := c.under
			hosted(t, under)
			rv, dir, oldLeaf := runFixture(t)
			id := ident.Identity{Vendor: "claude", Key: "fixture-thread"}
			if err := os.Rename(oldLeaf, filepath.Join(dir, "m", id.Digest()[:16])); err != nil {
				t.Fatal(err)
			}
			if c.offered {
				marker := fmt.Sprintf(`{"instance":"fixture-host","v":1,"pid":%d,"hello":[%q]}`, os.Getpid(), wire.HelloLaunchProof)
				if err := os.WriteFile(filepath.Join(dir, "ready"), []byte(marker), 0600); err != nil {
					t.Fatal(err)
				}
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
			for index, input := range []string{
				`{"hook_event_name":"Stop","session_id":"fixture-thread","prompt_id":"turn-0","stop_hook_active":false}`,
				`{"hook_event_name":"Notification","session_id":"fixture-thread","notification_type":"permission_prompt","prompt_id":"turn-1"}`,
				`{"hook_event_name":"Notification","session_id":"fixture-thread","notification_type":"idle_prompt"}`,
				`{"hook_event_name":"UserPromptSubmit","session_id":"fixture-thread","prompt_id":"turn-1"}`,
			} {
				var hookEvent struct {
					Name string `json:"hook_event_name"`
				}
				if err := json.Unmarshal([]byte(input), &hookEvent); err != nil {
					t.Fatal(err)
				}
				var out strings.Builder
				dispatchEvent(t.Context(), time.UnixMilli(200+int64(index)), rv, transport, []string{"claude", hookEvent.Name},
					strings.NewReader(input), &out, lookup)
			}
			// A late start marks the process as one a member's session started
			// in, as a start does, so a later /clear there is asked about.
			if launchMarked(rv, "72") != under {
				t.Fatalf("process marked %t, want %t", launchMarked(rv, "72"), under)
			}
			facts := []string{wire.HookTurnFinishing, wire.HookApprovalRequested, wire.HookIdlePrompt, wire.HookPromptSubmitted}
			if len(sent) != len(facts) {
				t.Fatalf("%d events sent, want %d", len(sent), len(facts))
			}
			for i, a := range sent {
				if a.Fact != facts[i] {
					t.Fatalf("event %d is a %s, want a %s", i, a.Fact, facts[i])
				}
				prompt := a.Fact != wire.HookApprovalRequested
				switch {
				case !under:
					if a.LaunchVerified || a.LaunchAtMS != 0 || a.PIDNamespace != "" {
						t.Fatalf("event %d outside the process: %+v", i, a)
					}
				case !a.LaunchVerified || a.LaunchRef != sent[0].LaunchRef:
					t.Fatalf("event %d reports launch %q (verified %t), want %q", i, a.LaunchRef, a.LaunchVerified, sent[0].LaunchRef)
				case !c.offered && (a.LaunchAtMS != 0 || a.PIDNamespace != ""):
					t.Fatalf("event %d carries the proof to a dispatcher that does not offer it: %+v", i, a)
				case c.offered && prompt && (a.LaunchAtMS != 200 || a.PIDNamespace != proc.PIDNamespace()):
					t.Fatalf("%s %d: launch_at_ms %d, pid_namespace %q", a.Fact, i, a.LaunchAtMS, a.PIDNamespace)
				case !prompt && (a.LaunchAtMS != 0 || a.PIDNamespace != ""):
					t.Fatalf("a %s carries the late-start proof: %+v", a.Fact, a)
				}
			}
		})
	}
}

// A prompt that binds a launch whose start was missed keeps the start
// budget: the dispatcher binds the launch in the prompt's request. So does
// a turn end that binds it (#437). A callback of a session nested under the
// process binds nothing and keeps the ordinary one.
func TestAPromptThatBindsAMissedStartKeepsTheStartBudget(t *testing.T) {
	for _, c := range []struct{ event, stdin string }{
		{"UserPromptSubmit", `{"hook_event_name":"UserPromptSubmit","session_id":"fixture-thread","prompt_id":"turn-0"}`},
		{"Stop", `{"hook_event_name":"Stop","session_id":"fixture-thread","prompt_id":"turn-0","stop_hook_active":false}`},
	} {
		for _, under := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s-hosted-%t", c.event, under), func(t *testing.T) { lateStartBudget(t, c.event, c.stdin, under) })
		}
	}
}

func lateStartBudget(t *testing.T, event, stdin string, under bool) {
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
	synctest.Test(t, func(t *testing.T) {
		entry := time.Now()
		var out strings.Builder
		prepare := func() (*os.Root, *hookCounter) {
			return rv, openHookCounter(rv, "claude", event, entry)
		}
		runTimed(entry, CallbackBase("claude", event), CallbackBudget("claude", event), &out, prepare,
			func(ctx context.Context, rv *os.Root, w io.Writer) string {
				return dispatchCallback(ctx, entry, rv, transport, []string{"claude", event}, strings.NewReader(stdin), w,
					lookup, callbackState{root: t.TempDir()})
			})
		time.Sleep(time.Second) // let a dispatch the deadline left behind finish
		raw, err := os.ReadFile(filepath.Join(dir, "hooks", entry.UTC().Format("20060102")))
		outcome := "rpc"
		if event == "Stop" {
			outcome = "clean" // a turn end publishes the bound launch without a reply
		}
		want, sent := "claude "+event+" "+outcome+" 200\n", 1
		if !under {
			want, sent = "claude "+event+" deadline 145\n", 0
		}
		if err != nil || string(raw) != want || events != sent {
			t.Fatalf("counter %q (%v) with %d events", raw, err, events)
		}
	})
}
