package hook

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func TestTurnEndEnvelopesContainOnlyPendingIDs(t *testing.T) {
	const line = "[crosshall] pending: 12 15"
	for vendor, want := range map[string]string{
		"claude": `{"hookSpecificOutput":{"hookEventName":"Stop","additionalContext":"[crosshall] pending: 12 15"}}`,
		"codex":  `{"decision":"block","reason":"[crosshall] pending: 12 15"}`,
	} {
		t.Run(vendor, func(t *testing.T) {
			m, _ := loadManifest(vendor)
			stop, _ := m.event("Stop")
			out, err := present(stop, wire.HookResult{TurnEndLine: line, PendingLine: "ordinary notice", ReminderText: "parked reminder"})
			if err != nil || string(out) != want+"\n" {
				t.Fatalf("native Stop output = %q (%v), want %s", out, err, want)
			}
			for _, invalid := range []string{"body", "[crosshall] pending: 0", "[crosshall] pending: 15 12", line + " message text", line + fixtureNoticeSuffix} {
				out, _ := present(stop, wire.HookResult{TurnEndLine: invalid})
				if len(out) != 0 {
					t.Errorf("invalid Stop line reached vendor: %q", out)
				}
			}
		})
	}
}

func TestTurnEndHookBoundaryFailsOpen(t *testing.T) {
	for _, tc := range []struct {
		name, event, flag, outcome string
		unknown, off, offline      bool
		calls                      int
		want, active, output       bool
		err                        error
		oldEpoch, unlisted         bool
	}{
		{name: "available", event: "Stop", flag: `,"stop_hook_active":false`, outcome: "rpc", calls: 1, want: true, output: true},
		{name: "continued elsewhere", event: "Stop", flag: `,"stop_hook_active":true`, outcome: "clean", calls: 1, active: true},
		{name: "missing flag", event: "Stop", outcome: "clean", calls: 1},
		{name: "check unavailable for this build", event: "Stop", flag: `,"stop_hook_active":false`, outcome: "clean", calls: 1, unlisted: true},
		{name: "wrong flag type", event: "Stop", flag: `,"stop_hook_active":"false"`, outcome: "clean", calls: 1},
		{name: "duplicate flag", event: "Stop", flag: `,"stop_hook_active":true,"stop_hook_active":false`, outcome: "clean", calls: 1},
		{name: "failure Stop", event: "StopFailure", flag: `,"stop_hook_active":false`, outcome: "clean", calls: 1},
		{name: "unknown build", event: "Stop", flag: `,"stop_hook_active":false`, outcome: "clean", calls: 1, unknown: true},
		{name: "setup off", event: "Stop", flag: `,"stop_hook_active":false`, outcome: "off", off: true},
		{name: "no dispatcher", event: "Stop", flag: `,"stop_hook_active":false`, outcome: "no_dispatcher", offline: true},
		{name: "transport timeout", event: "Stop", flag: `,"stop_hook_active":false`, outcome: "deadline", calls: 1, want: true, err: context.DeadlineExceeded},
		{name: "transport error", event: "Stop", flag: `,"stop_hook_active":false`, outcome: "refused", calls: 1, want: true, err: errors.New("lost response")},
		{name: "stale epoch", event: "Stop", flag: `,"stop_hook_active":false`, outcome: "stale_epoch", calls: 1, want: true, oldEpoch: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rv, dir, _ := runFixture(t)
			id := ident.Identity{Vendor: "claude", Key: "fixture-session"}
			leaf := filepath.Join(dir, "m", id.Digest()[:16])
			if err := os.Mkdir(leaf, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(leaf, "gen"), []byte("fixture-gen\n"), 0600); err != nil {
				t.Fatal(err)
			}
			policy := "claude 1 0 7\n"
			if tc.off {
				policy = "claude 0 0 7\n"
			}
			if err := os.WriteFile(filepath.Join(dir, "policy"), []byte(policy), 0600); err != nil {
				t.Fatal(err)
			}
			if !tc.unlisted {
				listed := "claude 2.1.327 " + runtime.GOOS + "-" + runtime.GOARCH + " print claude-2\n"
				if err := os.WriteFile(filepath.Join(dir, "turn_end"), []byte(listed), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if tc.offline {
				if err := os.Remove(filepath.Join(dir, "ready")); err != nil {
					t.Fatal(err)
				}
			}
			env := map[string]string{"AI_AGENT": "claude-code_2-1-327_harness", "CLAUDE_CODE_ENTRYPOINT": "sdk-cli"}
			if tc.unknown {
				delete(env, "AI_AGENT")
			}
			var calls int
			transport := recordedTransport{call: func(_ ident.Identity, _ string, req wire.Request, oneWay bool) (spool.Reply, error) {
				calls++
				var args wire.HookEventArgs
				if err := json.Unmarshal(req.Args, &args); err != nil || oneWay == tc.want || args.WantOutput != tc.want || args.ContinuationActive != tc.active {
					t.Fatalf("Stop request: one-way=%t, args=%s, error=%v", oneWay, req.Args, err)
				}
				if strings.Contains(string(req.Args), "private message") {
					t.Fatal("vendor text was copied into the hook request")
				}
				data := `{"epoch":7,"turn_end_line":"[crosshall] pending: 15"}`
				if tc.oldEpoch {
					data = `{"epoch":6,"turn_end_line":"[crosshall] pending: 15"}`
				}
				return spool.Reply{Response: wire.Response{Result: core.Result{Status: core.ExitOK, Data: json.RawMessage(data)}}}, tc.err
			}}
			ctx, cancel := context.WithTimeout(context.Background(), Budget)
			defer cancel()
			input := `{"hook_event_name":"` + tc.event + `","session_id":"fixture-session","prompt_id":"fixture-turn","last_assistant_message":"private message"` + tc.flag + `}`
			var out strings.Builder
			outcome := dispatchEvent(ctx, time.Now(), rv, transport, []string{"claude", tc.event}, strings.NewReader(input), &out,
				func(name string) (string, bool) { value, found := env[name]; return value, found })
			if calls != tc.calls || outcome != tc.outcome || (out.Len() != 0) != tc.output {
				t.Fatalf("calls=%d, outcome=%s, stdout=%q", calls, outcome, out.String())
			}
		})
	}
}
