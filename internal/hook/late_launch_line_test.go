package hook

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// A prompt that binds a launch whose start was missed asks the dispatcher
// for its pending line even when its stamp reads clean: while the session
// had no open launch, a new request did not mark its stamp, so the stamp
// says nothing about the work the session now owes.
func TestAPromptThatBindsAMissedStartAsksForItsLine(t *testing.T) {
	for _, under := range []bool{true, false} {
		t.Run(fmt.Sprintf("hosted-%t", under), func(t *testing.T) {
			hosted(t, under)
			rv, dir, oldLeaf := runFixture(t)
			id := ident.Identity{Vendor: "claude", Key: "fixture-thread"}
			leaf := filepath.Join(dir, "m", id.Digest()[:16])
			if err := os.Rename(oldLeaf, leaf); err != nil {
				t.Fatal(err)
			}
			for path, data := range map[string]string{
				filepath.Join(dir, "policy"):           "claude 1 0 7\n",
				filepath.Join(leaf, "stamp"):           "fixture-host fixture-gen 2 9\n",
				filepath.Join(leaf, seenReceiptNotice): "fixture-host fixture-gen 2\n",
			} {
				if err := os.WriteFile(path, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var oneWays []bool
			transport := recordedTransport{call: func(_ ident.Identity, _ string, req wire.Request, oneWay bool) (spool.Reply, error) {
				if req.Verb != wire.VerbHookEvent {
					return spool.Reply{}, nil
				}
				oneWays = append(oneWays, oneWay)
				result := wire.HookResult{Epoch: 7, PendingLine: "[crosshall] pending: 41"}
				data, err := json.Marshal(result)
				return spool.Reply{Response: wire.Response{Result: core.Result{Data: data}}}, err
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
			var out strings.Builder
			input := `{"hook_event_name":"UserPromptSubmit","session_id":"fixture-thread","prompt_id":"turn-0"}`
			outcome := dispatchEvent(t.Context(), time.UnixMilli(200), rv, transport, []string{"claude", "UserPromptSubmit"}, strings.NewReader(input), &out, lookup)
			asked := len(oneWays) == 1 && !oneWays[0]
			// Outside the process the prompt binds no launch: its stamp decides.
			if asked != under || strings.Contains(out.String(), "pending: 41") != under {
				t.Fatalf("calls %v, outcome %q, output %q; want asked=%t", oneWays, outcome, out.String(), under)
			}
		})
	}
}
