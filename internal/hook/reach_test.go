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
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func TestReachLineIsPrintedBeforePendingAndRecordedOnlyWhenPrinted(t *testing.T) {
	rv, _, leafPath := runFixture(t)
	leaf, err := ident.OpenPrivateDir(leafPath)
	if err != nil {
		t.Fatal(err)
	}
	defer leaf.Close()
	m, _ := loadManifest("codex")
	prompt, _ := m.event("UserPromptSubmit")
	start, _ := m.event("SessionStart")
	end, _ := m.event("Stop")
	ack := wire.HookReachAck{Generation: "fixture-gen", BindingID: 5, RouteEpoch: 1, Told: 1, Offer: "offer-1"}
	result := wire.HookResult{Epoch: 7, ReachLine: domain.ReachBringsLine, ReachAck: &ack, PendingLine: "[crosshall] pending: 12"}
	offer := func(a wire.HookReachAck) {
		t.Helper()
		raw, _ := json.Marshal(a)
		if err := leaf.WriteFile(ReachOfferFile, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}

	offer(ack)
	var out strings.Builder
	if failure := writeHookResult(t.Context(), rv, "codex", 7, prompt, result, &frameWriter{dst: &out}); failure != "" {
		t.Fatal(failure)
	}
	if want := domain.ReachBringsLine + `\n[crosshall] pending: 12`; !strings.Contains(out.String(), want) {
		t.Fatalf("prompt output %q, want the reach line before the pending line", out.String())
	}
	if !confirmReach(t.Context(), leaf, "fixture-gen", prompt, result) {
		t.Fatal("a printed line was not recorded")
	}
	if got, ok := ReadReachAck(leaf, "fixture-gen"); !ok || got != ack {
		t.Fatalf("recorded %+v %t", got, ok)
	}
	if _, ok := ReadReachAck(leaf, "replacement-gen"); ok {
		t.Fatal("the record outlived its generation")
	}

	lost := ack
	lost.Told, lost.Offer = 0, "offer-2"
	offer(lost)
	if confirmReach(t.Context(), leaf, "fixture-gen", prompt, result) {
		t.Fatal("a line no longer offered was recorded")
	}

	offer(ack)
	for _, tc := range []struct {
		name       string
		spec       eventSpec
		enrollment string
		shown      bool
	}{
		{"a turn's end", end, "", false},
		{"a first join", start, "joined", false},
		{"a restored start", start, "restored", true},
	} {
		r := result
		r.Enrollment = tc.enrollment
		if reachShown(tc.spec, r) != tc.shown {
			t.Fatalf("%s: shown %t", tc.name, !tc.shown)
		}
		if err := leaf.Remove(seenReach); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if confirmReach(t.Context(), leaf, "fixture-gen", tc.spec, r) != tc.shown {
			t.Fatalf("%s: recorded a line it did not print, or lost one it did", tc.name)
		}
	}
}

// The dispatcher moves the notice stamp when a session's reach changes, so a
// Claude Code prompt with a messaging socket keeps the clean path (#121).
func TestAPromptWithAMessagingSocketKeepsTheCleanPath(t *testing.T) {
	for _, socket := range []bool{false, true} {
		t.Run(fmt.Sprintf("socket %t", socket), func(t *testing.T) {
			rv, dir, oldLeaf := runFixture(t)
			id := ident.Identity{Vendor: "claude", Key: "fixture-thread"}
			leaf := filepath.Join(dir, "m", id.Digest()[:16])
			if err := os.Rename(oldLeaf, leaf); err != nil {
				t.Fatal(err)
			}
			for path, data := range map[string]string{
				filepath.Join(dir, "policy"): "claude 1 0 7\n",
				filepath.Join(leaf, "stamp"): "fixture-host fixture-gen 2 9\n",
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
				result := wire.HookResult{Epoch: 7, Ack: &wire.HookAck{Kind: "notice", Instance: "fixture-host", Generation: "fixture-gen", Seq: 2}}
				data, err := json.Marshal(result)
				return spool.Reply{Response: wire.Response{Result: core.Result{Data: data}}}, err
			}}
			lookup := func(key string) (string, bool) {
				switch key {
				case "CLAUDE_PID":
					return "72", true
				case "CLAUDE_CODE_SESSION_ID":
					return "fixture-thread", true
				case "CLAUDE_CODE_MESSAGING_SOCKET":
					return "/tmp/cc-socks/72.sock", socket
				}
				return "", false
			}
			for i, turn := range []string{"first", "second", "third"} {
				input := fmt.Sprintf(`{"hook_event_name":"UserPromptSubmit","session_id":"fixture-thread","prompt_id":%q}`, turn)
				var out strings.Builder
				dispatchEvent(t.Context(), time.UnixMilli(200+int64(i)), rv, transport, []string{"claude", "UserPromptSubmit"},
					strings.NewReader(input), &out, lookup)
			}
			// The first prompt asks and records the notice stamp.
			want := []bool{false, true, true}
			if fmt.Sprint(oneWays) != fmt.Sprint(want) {
				t.Fatalf("one-way prompts %v, want %v", oneWays, want)
			}
		})
	}
}
