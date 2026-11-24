package hook

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

// A start that would join sends start, in one round trip, when the
// dispatcher offers it; otherwise it keeps the lookup, and after an older
// dispatcher's lookup the join.
func TestAStartThatWouldJoinSendsStartWhenTheDispatcherOffersIt(t *testing.T) {
	for _, tc := range []struct {
		name            string
		offered, target bool
		refuse          core.Code
		calls, outcome  string
	}{
		{"offered, joins", true, true, "", "start,hook.enroll", "rpc"},
		{"offered, outside any repository", true, false, "", "lookup", "unmatched"},
		{"offered, a retired key", true, true, core.CodeKeyRetired, "start", "refused"},
		{"an older dispatcher", false, true, "", "lookup,join,hook.enroll", "rpc"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rv, dir, leaf := runFixture(t)
			parked := filepath.Join(dir, "parked")
			if err := os.Rename(leaf, parked); err != nil {
				t.Fatal(err)
			}
			if tc.offered {
				marker := fmt.Sprintf(`{"instance":"fixture-host","v":1,"pid":%d,"hello":["start"]}`+"\n", os.Getpid())
				if err := os.WriteFile(filepath.Join(dir, "ready"), []byte(marker), 0600); err != nil {
					t.Fatal(err)
				}
			}
			cwd := dir
			if tc.target {
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
				index := fmt.Sprintf("%x\n", sha256.Sum256([]byte(repo)))
				if err := os.WriteFile(filepath.Join(dir, "repo-defaults"), []byte(index), 0600); err != nil {
					t.Fatal(err)
				}
				cwd = repo
			}
			var order []string
			transport := recordedTransport{
				lookup: func(intent string) (string, wire.Response, error) {
					order = append(order, intent)
					switch {
					case tc.refuse != "":
						return "", wire.Response{Result: core.Result{Status: core.ExitNotEnrolled, Code: tc.refuse}}, nil
					case intent == "lookup":
						return "", wire.Response{Result: core.Result{Status: core.ExitNotEnrolled, Code: core.CodeNotEnrolled}}, nil
					}
					if err := os.Rename(parked, leaf); err != nil {
						t.Fatal(err)
					}
					return "fixture-gen", wire.Response{Result: core.Result{Status: core.ExitOK}}, nil
				},
				call: func(_ ident.Identity, _ string, req wire.Request, _ bool) (spool.Reply, error) {
					order = append(order, req.Verb)
					data, err := json.Marshal(wire.HookResult{Enrollment: "joined", Epoch: 7, Channel: "dev"})
					return spool.Reply{Response: wire.Response{Result: core.Result{Status: core.ExitOK, Data: data}}}, err
				},
			}
			_, outcome := simulate(t, rv, transport, "SessionStart", map[string]any{"cwd": cwd, "source": "startup"})
			if got := strings.Join(order, ","); got != tc.calls || outcome != tc.outcome {
				t.Fatalf("calls %s, outcome %s; want %s, %s", got, outcome, tc.calls, tc.outcome)
			}
		})
	}
}
