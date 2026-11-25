package hook

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
// route (#252). No other callback sends them, and no prompt sends them to a
// dispatcher that does not offer to accept them: an older one decodes
// hook.event strictly and would refuse the prompt.
func TestOnlyACodexPromptNamesItsServerAndOnlyWhenOffered(t *testing.T) {
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
	for _, offered := range []bool{true, false} {
		t.Run(fmt.Sprintf("offered-%t", offered), func(t *testing.T) {
			rv, dir, _ := runFixture(t)
			if offered {
				marker := fmt.Sprintf(`{"instance":"fixture-host","v":1,"pid":%d,"hello":[%q,%q]}`, os.Getpid(),
					wire.HelloLaunchProof, wire.HelloPromptHost)
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
				named := offered && e.args.Fact == wire.HookPromptSubmitted
				if named && (e.args.Host == nil || *e.args.Host != want || e.args.VendorHome != home) {
					t.Fatalf("prompt %d names %+v in %q, want %+v in %q", i, e.args.Host, e.args.VendorHome, want, home)
				}
				if !named && (hasHost || hasHome) {
					t.Fatalf("event %d (%s, offered %t) carries a server: %s", i, e.args.Fact, offered, e.keys["host"])
				}
			}
		})
	}
}
