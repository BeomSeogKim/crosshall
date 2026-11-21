package hook

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/hook/manifests"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/proc"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

type recordedTransport struct {
	lookup func(string) (string, wire.Response, error)
	call   func(ident.Identity, string, wire.Request, bool) (spool.Reply, error)
}

func (r recordedTransport) HookLookup(_ context.Context, _ ident.Identity, intent string) (string, wire.Response, error) {
	return r.lookup(intent)
}
func (r recordedTransport) HookCall(_ context.Context, id ident.Identity, generation string, req wire.Request, oneWay bool) (spool.Reply, error) {
	return r.call(id, generation, req, oneWay)
}

func runFixture(t *testing.T) (*os.Root, string, string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	id := ident.Identity{Vendor: "codex", Key: "fixture-thread"}
	leaf := filepath.Join(dir, "m", id.Digest()[:16])
	if err := os.MkdirAll(leaf, 0700); err != nil {
		t.Fatal(err)
	}
	for path, data := range map[string]string{
		filepath.Join(leaf, "gen"):   "fixture-gen\n",
		filepath.Join(dir, "ready"):  fmt.Sprintf("{\"instance\":\"fixture-host\",\"v\":1,\"pid\":%d}\n", os.Getpid()),
		filepath.Join(dir, "policy"): "codex 1 0 7\n",
	} {
		if err := os.WriteFile(path, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
	}
	rv, err := ident.OpenPrivateDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rv.Close() })
	return rv, dir, leaf
}

func simulate(t *testing.T, rv *os.Root, transport recordedTransport, nativeEvent string, fields map[string]any) (string, string) {
	t.Helper()
	fields["session_id"] = "fixture-thread"
	fields["hook_event_name"] = nativeEvent
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	outcome := dispatchEvent(t.Context(), time.Now(), rv, transport, []string{"codex", nativeEvent}, strings.NewReader(string(raw)), &out, func(string) (string, bool) { return "", false })
	return out.String(), outcome
}

func TestHookBoundaryUsesOnlyCorePresentationAndCurrentEpoch(t *testing.T) {
	for _, tc := range []struct {
		name, data, want string
		changePolicy     bool
	}{
		{"offer", `{"epoch":7,"pending_line":"[crosshall] pending: 12 14"}`, "[crosshall] pending: 12 14", false},
		{"replay", `{"epoch":7}`, "", false},
		{"old response", `{"epoch":6,"pending_line":"[crosshall] pending: 12"}`, "", false},
		{"disabled while waiting", `{"epoch":7,"pending_line":"[crosshall] pending: 12"}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rv, dir, _ := runFixture(t)
			transport := recordedTransport{call: func(id ident.Identity, gen string, req wire.Request, oneWay bool) (spool.Reply, error) {
				if oneWay || req.Verb != "hook.event" || id.Key != "fixture-thread" || gen != "fixture-gen" {
					t.Fatalf("wrong request: %v %q %v", req.Verb, gen, oneWay)
				}
				var args map[string]any
				if err := json.Unmarshal(req.Args, &args); err != nil {
					t.Fatal(err)
				}
				if args["want_output"] != true || args["epoch"] != float64(7) || args["fact"] != "prompt_submitted" || args["turn_ref"] != "fixture-turn" || args["launch_ref"] != "session" {
					t.Fatalf("wrong args %s", req.Args)
				}
				if strings.Contains(string(req.Args), "fixture-private-prompt") {
					t.Fatal("prompt reached exchange")
				}
				if tc.changePolicy {
					if err := os.WriteFile(filepath.Join(dir, "policy"), []byte("codex 0 0 8\n"), 0600); err != nil {
						t.Fatal(err)
					}
				}
				return spool.Reply{Response: wire.Response{Result: core.Result{Status: core.ExitOK, Data: json.RawMessage(tc.data)}}}, nil
			}}
			out, _ := simulate(t, rv, transport, "UserPromptSubmit", map[string]any{"turn_id": "fixture-turn", "prompt": "fixture-private-prompt"})
			if tc.want == "" {
				if out != "" {
					t.Fatalf("unexpected output: %s", out)
				}
				return
			}
			var host struct {
				Payload struct {
					Event   string `json:"hookEventName"`
					Context string `json:"additionalContext"`
				} `json:"hookSpecificOutput"`
			}
			if err := json.Unmarshal([]byte(out), &host); err != nil || host.Payload.Context != tc.want || host.Payload.Event != "UserPromptSubmit" {
				t.Fatalf("unexpected boundary: %q (%v)", out, err)
			}
		})
	}
}

func TestHookObservesWithoutOutputOnEnd(t *testing.T) {
	rv, _, _ := runFixture(t)
	calls := 0
	transport := recordedTransport{call: func(_ ident.Identity, _ string, req wire.Request, oneWay bool) (spool.Reply, error) {
		calls++
		if !oneWay || !strings.Contains(string(req.Args), `"want_output":false`) {
			t.Fatalf("expected one-way end: %s", req.Args)
		}
		return spool.Reply{}, nil
	}}
	out, _ := simulate(t, rv, transport, "SessionEnd", map[string]any{"reason": "other"})
	if calls != 1 || out != "" {
		t.Fatalf("end: %d, %q", calls, out)
	}
}

func TestMissingLeafStartSkipsLookupWithoutDispatcher(t *testing.T) {
	rv, dir, leaf := runFixture(t)
	if err := os.Rename(leaf, filepath.Join(dir, "parked")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "ready")); err != nil {
		t.Fatal(err)
	}
	transport := recordedTransport{lookup: func(intent string) (string, wire.Response, error) {
		t.Fatalf("stopped dispatcher received first-contact lookup %q", intent)
		return "", wire.Response{}, nil
	}}
	out, outcome := simulate(t, rv, transport, "SessionStart", map[string]any{"source": "startup", "cwd": dir})
	if out != "" || outcome != "no_dispatcher" {
		t.Fatalf("outcome %q, output %q", outcome, out)
	}
}

func TestMissingLeafStartOnlyChecksDefaultAfterLookupRefusal(t *testing.T) {
	for _, tc := range []struct {
		code            core.Code
		source, outcome string
		wantHint        bool
	}{
		{core.CodeKeyRetired, "startup", "refused", false},
		{core.CodeNotEnrolled, "startup", "unmatched", true},
		{core.CodeNotEnrolled, "compact", "unmatched", false},
		{core.CodeNotEnrolled, "resume", "unmatched", false},
	} {
		t.Run(string(tc.code)+"/"+tc.source, func(t *testing.T) {
			rv, dir, leaf := runFixture(t)
			if err := os.RemoveAll(leaf); err != nil {
				t.Fatal(err)
			}
			transport := recordedTransport{lookup: func(intent string) (string, wire.Response, error) {
				if intent != "lookup" {
					t.Fatalf("unmatched or retired key attempted %s", intent)
				}
				return "", wire.Response{Result: core.Result{Status: core.ExitNotEnrolled, Code: tc.code}}, nil
			}}
			out, result := simulate(t, rv, transport, "SessionStart", map[string]any{"source": tc.source, "cwd": dir})
			if (out != "") != tc.wantHint || result != tc.outcome {
				t.Fatalf("outcome %q, output %q", result, out)
			}
			if _, err := os.Stat(leaf); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("created unmatched leaf")
			}
		})
	}
}

func TestStartRestoresOutsideRepoAndJoinsOnlyThroughDefault(t *testing.T) {
	for _, restore := range []bool{true, false} {
		t.Run(fmt.Sprintf("restore-%t", restore), func(t *testing.T) {
			rv, dir, leaf := runFixture(t)
			// Model a missing leaf by moving the private fixture out of the member tree.
			parked := filepath.Join(dir, "parked")
			if err := os.Rename(leaf, parked); err != nil {
				t.Fatal(err)
			}
			cwd, repo := dir, ""
			if !restore {
				var err error
				repo, err = filepath.EvalSymlinks(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				cwd = filepath.Join(repo, "src")
				if err := os.Mkdir(cwd, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Mkdir(filepath.Join(repo, ".git"), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0600); err != nil {
					t.Fatal(err)
				}
				index := fmt.Sprintf("%x\n", sha256.Sum256([]byte(repo)))
				if err := os.WriteFile(filepath.Join(dir, "repo-defaults"), []byte(index), 0600); err != nil {
					t.Fatal(err)
				}
			}
			var order []string
			transport := recordedTransport{
				lookup: func(intent string) (string, wire.Response, error) {
					order = append(order, intent)
					if intent == "lookup" && !restore {
						return "", wire.Response{Result: core.Result{Status: core.ExitNotEnrolled, Code: core.CodeNotEnrolled}}, nil
					}
					if err := os.Rename(parked, leaf); err != nil {
						t.Fatal(err)
					}
					return "fixture-gen", wire.Response{Result: core.Result{Status: core.ExitOK}}, nil
				},
				call: func(_ ident.Identity, gen string, req wire.Request, oneWay bool) (spool.Reply, error) {
					order = append(order, req.Verb)
					var a wire.HookEnrollArgs
					if err := json.Unmarshal(req.Args, &a); err != nil {
						t.Fatal(err)
					}
					if oneWay || gen != "fixture-gen" || a.RepoRoot != repo || a.Source != "startup" || a.LaunchVerified || a.Profile.VendorVersion != "unverified" || a.Profile.Mode != "unverified" || a.Epoch != 7 {
						t.Fatalf("unexpected enrollment: %s", req.Args)
					}
					enrollment := "joined"
					if restore {
						enrollment = "restored"
					}
					data, err := json.Marshal(wire.HookResult{Enrollment: enrollment, Epoch: 7, Channel: "dev", Hint: "welcome fixture", Roster: json.RawMessage(`[{"alias":"member","state":"unknown","vendor":"codex"}]`), PendingLine: "[crosshall] pending: 4"})
					return spool.Reply{Response: wire.Response{Result: core.Result{Status: core.ExitOK, Data: data}}}, err
				},
			}
			out, outcome := simulate(t, rv, transport, "SessionStart", map[string]any{"cwd": cwd, "source": "startup"})
			calls := "lookup,join,hook.enroll"
			if restore {
				calls = "lookup,hook.enroll"
			}
			if outcome != "rpc" || strings.Join(order, ",") != calls || !strings.Contains(out, "[crosshall] pending: 4") || strings.Contains(out, "welcome fixture") == restore || strings.Contains(out, "@member") == restore {
				t.Fatalf("calls %v, outcome %s, output %q", order, outcome, out)
			}
		})
	}
}

func TestDirtyPromptAckAndUnverifiedStopsStayOneWay(t *testing.T) {
	rv, dir, leaf := runFixture(t)
	if err := os.WriteFile(filepath.Join(leaf, "stamp"), []byte("fixture-host fixture-gen 2 9\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// The parked reminder policy does not activate the turn-end check.
	if err := os.WriteFile(filepath.Join(dir, "policy"), []byte("codex 1 1 7\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var oneWays []bool
	transport := recordedTransport{call: func(_ ident.Identity, _ string, req wire.Request, oneWay bool) (spool.Reply, error) {
		oneWays = append(oneWays, oneWay)
		var a wire.HookEventArgs
		if err := json.Unmarshal(req.Args, &a); err != nil {
			t.Fatal(err)
		}
		result := wire.HookResult{Epoch: 7}
		if a.Fact == wire.HookPromptSubmitted && !oneWay {
			result.Ack = &wire.HookAck{Kind: "notice", Instance: "fixture-host", Generation: "fixture-gen", Seq: 2}
		} else if a.Fact == wire.HookTurnFinishing {
			result.Ack = &wire.HookAck{Kind: "reminder", Instance: "fixture-host", Generation: "fixture-gen", Seq: 9, TurnRef: a.TurnRef}
			result.ReminderText = "unavailable in slice one"
		}
		data, err := json.Marshal(result)
		return spool.Reply{Response: wire.Response{Result: core.Result{Data: data}}}, err
	}}
	for _, callback := range []struct {
		name, turn string
		clean      bool
	}{
		{"UserPromptSubmit", "first", false},
		{"UserPromptSubmit", "second", true},
		{"Stop", "second", true},
		{"Stop", "second", true},
		{"Stop", "third", true},
	} {
		out, outcome := simulate(t, rv, transport, callback.name, map[string]any{"turn_id": callback.turn, "stop_hook_active": false})
		if out != "" || len(oneWays) == 0 || oneWays[len(oneWays)-1] != callback.clean || (outcome == "clean") != callback.clean {
			t.Fatalf("%s/%s: calls %v, outcome %q, output %q", callback.name, callback.turn, oneWays, outcome, out)
		}
	}
}

func TestClaudeStartAndEndCarryTheSameVerifiedReference(t *testing.T) {
	rv, dir, oldLeaf := runFixture(t)
	id := ident.Identity{Vendor: "claude", Key: "fixture-thread"}
	leaf := filepath.Join(dir, "m", id.Digest()[:16])
	if err := os.Rename(oldLeaf, leaf); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "policy"), []byte("claude 1 0 7\n"), 0600); err != nil {
		t.Fatal(err)
	}
	var labels []string
	transport := recordedTransport{call: func(_ ident.Identity, _ string, req wire.Request, oneWay bool) (spool.Reply, error) {
		if req.Verb == wire.VerbHookRoute {
			return spool.Reply{}, nil // the start's socket report carries no profile
		}
		var a struct {
			Profile      wire.HookProfile `json:"profile"`
			Launch       string           `json:"launch_ref"`
			Verified     bool             `json:"launch_verified"`
			PIDNamespace string           `json:"pid_namespace"`
		}
		if err := json.Unmarshal(req.Args, &a); err != nil {
			t.Fatal(err)
		}
		if !a.Verified || a.Launch == "" || a.Launch == "72" || a.Profile.VendorVersion != "unverified" || a.Profile.Mode != "unverified" {
			t.Fatalf("lost the distinction between launch identity and build evidence: %s", req.Args)
		}
		var namespace string
		if req.Verb == wire.VerbHookEnroll {
			namespace = proc.PIDNamespace()
		}
		if a.PIDNamespace != namespace {
			t.Fatalf("%s namespace = %q, want %q", req.Verb, a.PIDNamespace, namespace)
		}
		labels = append(labels, a.Launch)
		return spool.Reply{Response: wire.Response{Result: core.Result{Data: json.RawMessage(`{"epoch":7,"enrollment":"restored"}`)}}}, nil
	}}
	lookup := func(key string) (string, bool) {
		switch key {
		case "CLAUDE_PID":
			return "72", true
		case "CLAUDE_CODE_SESSION_ID":
			return "fixture-thread", true
		default:
			return "", false
		}
	}
	for index, name := range []string{"SessionStart", "SessionEnd", "SessionStart", "SessionEnd"} {
		input := fmt.Sprintf(`{"hook_event_name":%q,"session_id":"fixture-thread","source":"startup","reason":"other"}`, name)
		var out strings.Builder
		result := dispatchEvent(t.Context(), time.UnixMilli(200+int64(index)), rv, transport, []string{"claude", name}, strings.NewReader(input), &out, lookup)
		// A start waits for its enrollment; an end is published one-way.
		want := "rpc"
		if name == "SessionEnd" {
			want = "clean"
		}
		if result != want || out.Len() != 0 {
			t.Fatalf("%s returned %s, %s", name, result, out.String())
		}
	}
	if len(labels) != 4 || labels[0] != labels[1] || labels[2] != labels[3] || labels[0] == labels[2] {
		t.Fatalf("two launches reused a pid: %q", labels)
	}
}

func TestDisabledAndMissingPolicySkipInputAndTransport(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(fmt.Sprintf("missing-%t", missing), func(t *testing.T) {
			rv, dir, _ := runFixture(t)
			path := filepath.Join(dir, "policy")
			if missing {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else if err := os.WriteFile(path, []byte("codex 0 0 8\n"), 0600); err != nil {
				t.Fatal(err)
			}
			var out strings.Builder
			outcome := dispatchEvent(t.Context(), time.Now(), rv, recordedTransport{}, []string{"codex", "SessionStart"}, nil, &out, nil)
			if outcome != "off" || out.Len() != 0 {
				t.Fatalf("outcome %s, output %q", outcome, out.String())
			}
		})
	}
}

// Every shipped manifest's hook passes the policy gate when the dispatcher
// lists its adapter as enabled; no adapter is named here.
func TestEveryManifestHookEntersWhenItsPolicyIsOn(t *testing.T) {
	var policy strings.Builder
	for _, name := range manifests.Names() {
		policy.WriteString(name + " 1 0 7\n")
	}
	for _, name := range manifests.Names() {
		m, ok := loadManifest(name)
		if !ok {
			t.Fatalf("manifest %s does not load", name)
		}
		for event := range m.Events {
			rv, dir, _ := runFixture(t)
			if err := os.WriteFile(filepath.Join(dir, "policy"), []byte(policy.String()), 0600); err != nil {
				t.Fatal(err)
			}
			var out strings.Builder
			outcome := dispatchEvent(t.Context(), time.Now(), rv, recordedTransport{}, []string{name, event}, strings.NewReader("{}"), &out, nil)
			if outcome == "off" { // past the gate the empty input is refused
				t.Fatalf("%s %s: hook outcome %s with its policy on", name, event, outcome)
			}
			break
		}
	}
}
