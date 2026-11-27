package hook

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/proc"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// A Codex prompt run by the managed server names that server and its home,
// as a start does, so a dispatcher that missed the start can register the
// route (#252); a turn end does too (#437). No other callback sends them,
// and neither sends them to a dispatcher that does not offer to accept them:
// an older one decodes hook.event strictly and would refuse the callback.
func TestOnlyACodexPromptOrTurnEndNamesItsServerAndOnlyWhenOffered(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := filepath.Join(home, "packages", "app-server-daemon", "releases", "0.171.0-aarch64-apple-darwin", "bin", "codex")
	if err := os.MkdirAll(filepath.Dir(server), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(server, nil, 0o700); err != nil {
		t.Fatal(err)
	}
	standInHost(t, server, []string{server, "app-server", "--managed-daemon"}, true)
	for _, tc := range []struct {
		name  string
		hello []string
	}{
		{"no offer", nil},
		{"prompt only", []string{wire.HelloLaunchProof, wire.HelloPromptHost}},
		{"prompt and turn end", []string{wire.HelloLaunchProof, wire.HelloPromptHost, wire.HelloTurnHost}},
	} {
		offered := tc.hello != nil
		t.Run(tc.name, func(t *testing.T) {
			rv, dir, _ := runFixture(t)
			if offered {
				hello, err := json.Marshal(tc.hello)
				if err != nil {
					t.Fatal(err)
				}
				marker := fmt.Sprintf(`{"instance":"fixture-host","v":1,"pid":%d,"hello":%s}`, os.Getpid(), hello)
				if err := os.WriteFile(filepath.Join(dir, "ready"), []byte(marker), 0600); err != nil {
					t.Fatal(err)
				}
			}
			type sent struct {
				args wire.HookEventArgs
				keys map[string]json.RawMessage
			}
			var events []sent
			transport := recordedTransport{call: func(_ ident.Identity, _ string, req wire.Request, _ bool) (spool.Reply, error) {
				if req.Verb == wire.VerbHookEvent {
					var s sent
					if err := json.Unmarshal(req.Args, &s.args); err != nil {
						t.Fatal(err)
					}
					if err := json.Unmarshal(req.Args, &s.keys); err != nil {
						t.Fatal(err)
					}
					events = append(events, s)
				}
				return spool.Reply{}, nil
			}}
			lookup := environment(map[string]string{"CODEX_THREAD_ID": "fixture-thread", "CODEX_HOME": home})
			for index, name := range []string{"UserPromptSubmit", "Stop", "UserPromptSubmit"} {
				input := fmt.Sprintf(`{"hook_event_name":%q,"session_id":"fixture-thread","turn_id":"turn-%d"}`, name, index)
				var out strings.Builder
				dispatchEvent(t.Context(), time.UnixMilli(200+int64(index)), rv, transport, []string{"codex", name}, strings.NewReader(input), &out, lookup)
				if out.Len() != 0 && !offered {
					t.Fatalf("%s printed %q to an older dispatcher's session", name, out.String())
				}
			}
			if len(events) != 3 {
				t.Fatalf("%d events sent, want 3", len(events))
			}
			want := wire.Host{PID: standInPID, Start: standInStart, NS: proc.PIDNamespace()}
			for i, e := range events {
				_, hasHost := e.keys["host"]
				_, hasHome := e.keys["vendor_home"]
				named := e.args.Fact == wire.HookPromptSubmitted && slices.Contains(tc.hello, wire.HelloPromptHost) ||
					e.args.Fact == wire.HookTurnFinishing && slices.Contains(tc.hello, wire.HelloTurnHost)
				if named && (e.args.Host == nil || *e.args.Host != want || e.args.VendorHome != home) {
					t.Fatalf("event %d (%s) names %+v in %q, want %+v in %q", i, e.args.Fact, e.args.Host, e.args.VendorHome, want, home)
				}
				if !named && (hasHost || hasHome) {
					t.Fatalf("event %d (%s, offered %t) carries a server: %s", i, e.args.Fact, offered, e.keys["host"])
				}
			}
		})
	}
}
