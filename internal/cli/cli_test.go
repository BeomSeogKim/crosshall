package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
)

type fakeTransport struct {
	calls  []Call
	result core.Result
}

func (f *fakeTransport) Do(_ context.Context, call Call) (core.Result, error) {
	f.calls = append(f.calls, call)
	return f.result, nil
}

func run(t *testing.T, env Env, args ...string) (string, string, core.ExitCode) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := RunEnv(context.Background(), env, args, &out, &errOut)
	return out.String(), errOut.String(), code
}

func okResult(t *testing.T, data any) core.Result {
	t.Helper()
	r, err := domain.OK(data)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestJoinParsesArgumentsAndKey(t *testing.T) {
	ft := &fakeTransport{result: okResult(t, domain.JoinResult{Channel: "checkout", Alias: "review"})}
	_, stderr, code := run(t, Env{Transport: ft}, "--key", "k1", "join", "#checkout", "--as", "@review", "--focus=refund rounding")
	if code != core.ExitOK {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	want := domain.Join{Channel: "checkout", Alias: "review", Focus: "refund rounding"}
	if len(ft.calls) != 1 || ft.calls[0].Command != want || ft.calls[0].OpKey != "k1" || ft.calls[0].Owner {
		t.Fatalf("wrong call: %+v", ft.calls)
	}
}

func TestDurableCommandsGetAFreshKey(t *testing.T) {
	ft := &fakeTransport{result: okResult(t, domain.JoinResult{Channel: "c", Alias: "a"})}
	run(t, Env{Transport: ft}, "join", "c")
	run(t, Env{Transport: ft}, "join", "c")
	if len(ft.calls) != 2 || len(ft.calls[0].OpKey) != 32 || ft.calls[0].OpKey == ft.calls[1].OpKey {
		t.Fatalf("keys not fresh: %+v", ft.calls)
	}
	ft.calls = nil
	ft.result = okResult(t, domain.WhoResult{Channel: "c"})
	run(t, Env{Transport: ft}, "who")
	if ft.calls[0].OpKey != "" {
		t.Fatalf("who should carry no key, got %q", ft.calls[0].OpKey)
	}
}

func TestUsageErrors(t *testing.T) {
	ft := &fakeTransport{}
	for _, args := range [][]string{
		{"join", "Bad_Name"},
		{"join", "a", "b"},
		{"join", "a", "--as"},
		{"join", "a", "--bogus"},
		{"who", "extra"},
		{"owner", "channel", "create"},
		{"owner", "nonsense"},
		{"run"},
		{"run", "ls"},
		{"--key"},
		{"frobnicate"},
	} {
		if _, _, code := run(t, Env{Transport: ft}, args...); code != core.ExitUsage {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if len(ft.calls) != 0 {
		t.Fatalf("usage errors reached the transport: %+v", ft.calls)
	}
}

// A member's help lists what a member can run. The owner's commands are one
// step away, and reading them needs no owner terminal.
func TestHelpKeepsOwnerCommandsApart(t *testing.T) {
	for _, args := range [][]string{{"help"}, {}} {
		out, _, code := run(t, Env{}, args...)
		if code != core.ExitOK || !strings.Contains(out, "\n  reply <id>") || !strings.Contains(out, "\n  owner help ") {
			t.Fatalf("%v: member help (%d):\n%s", args, code, out)
		}
		for _, owner := range []string{"owner start", "owner budget", "setup", "uninstall"} {
			if strings.Contains(out, owner) {
				t.Fatalf("%v: member help names %q:\n%s", args, owner, out)
			}
		}
	}
	out, _, code := run(t, Env{OwnerGate: func() string { return "CLAUDE_CODE_SESSION_ID is set" }}, "owner", "help")
	if code != core.ExitOK || strings.Contains(out, "\n  reply <id>") {
		t.Fatalf("owner help (%d):\n%s", code, out)
	}
	for _, owner := range []string{"owner start", "owner budget grant", "owner adapter evidence", "\n  setup [--dry-run]", "\n  uninstall "} {
		if !strings.Contains(out, owner) {
			t.Fatalf("owner help lacks %q:\n%s", owner, out)
		}
	}
	if _, stderr, code := run(t, Env{}, "owner"); code != core.ExitUsage || !strings.Contains(stderr, "crosshall owner help") {
		t.Fatalf("bare owner (%d): %s", code, stderr)
	}
}

func TestOwnerGateRefusesWithoutPublishing(t *testing.T) {
	ft := &fakeTransport{}
	env := Env{Transport: ft, OwnerGate: func() string { return "CLAUDE_CODE_SESSION_ID is set" }}
	stdout, _, code := run(t, env, "--json", "owner", "channel", "create", "checkout")
	if code != core.ExitRefused || len(ft.calls) != 0 {
		t.Fatalf("exit %d, calls %d", code, len(ft.calls))
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(stdout), &got); err != nil {
		t.Fatalf("not one JSON object: %q", stdout)
	}
	if got["ok"] != false || got["code"] != "owner_only" {
		t.Fatalf("wrong refusal: %v", got)
	}
}

func TestOwnerChannelCreateIsOwnerCall(t *testing.T) {
	ft := &fakeTransport{result: okResult(t, domain.ChannelResult{Channel: "checkout", JoinPool: 8})}
	stdout, _, code := run(t, Env{Transport: ft, OwnerGate: func() string { return "" }},
		"owner", "channel", "create", "checkout", "--topic", "payment screen")
	if code != core.ExitOK || stdout != "#checkout created. Its join pool holds 8 root token(s): each member's first join takes one.\n" {
		t.Fatalf("exit %d, stdout %q", code, stdout)
	}
	c := ft.calls[0]
	if !c.Owner || c.Command != (domain.CreateChannel{Name: "checkout", Topic: "payment screen"}) || c.OpKey == "" {
		t.Fatalf("wrong call: %+v", c)
	}
}

func TestOwnerChannelCreateTakesAPool(t *testing.T) {
	ft := &fakeTransport{result: okResult(t, domain.ChannelResult{Channel: "practice"})}
	stdout, _, code := run(t, Env{Transport: ft, OwnerGate: func() string { return "" }},
		"owner", "channel", "create", "practice", "--pool", "0")
	if code != core.ExitOK || stdout != "#practice created, with no join pool.\n" {
		t.Fatalf("exit %d, stdout %q", code, stdout)
	}
	if cmd, ok := ft.calls[0].Command.(domain.CreateChannel); !ok || cmd.Pool == nil || *cmd.Pool != 0 {
		t.Fatalf("wrong call: %+v", ft.calls[0])
	}
	for _, bad := range []string{"-1", "x", ""} {
		if _, _, code := run(t, Env{Transport: &fakeTransport{}, OwnerGate: func() string { return "" }},
			"owner", "channel", "create", "practice", "--pool", bad); code != core.ExitUsage {
			t.Fatalf("--pool %q: exit %d", bad, code)
		}
	}
}

func TestJSONSuccessIsOneObjectWithKind(t *testing.T) {
	ft := &fakeTransport{result: okResult(t, domain.WhoResult{
		Channel: "checkout",
		Members: []domain.Member{{Alias: "form", Vendor: "claude", State: "unknown", Route: "pull"}},
	})}
	stdout, _, code := run(t, Env{Transport: ft}, "who", "--json")
	if code != core.ExitOK {
		t.Fatalf("exit %d", code)
	}
	dec := json.NewDecoder(strings.NewReader(stdout))
	var got map[string]any
	if err := dec.Decode(&got); err != nil || dec.More() {
		t.Fatalf("want exactly one object, got %q", stdout)
	}
	if got["v"] != float64(1) || got["ok"] != true || got["kind"] != "who" || got["channel"] != "checkout" {
		t.Fatalf("wrong object: %v", got)
	}
}

func TestRefusalPlainAndExit(t *testing.T) {
	ft := &fakeTransport{result: domain.Refuse(core.CodeOneChannel, "already a member of #a", "crosshall who")}
	stdout, stderr, code := run(t, Env{Transport: ft}, "join", "b")
	if code != core.ExitRefused || stdout != "" {
		t.Fatalf("exit %d stdout %q", code, stdout)
	}
	if !strings.Contains(stderr, "already a member of #a") || !strings.Contains(stderr, "next: crosshall who") {
		t.Fatalf("stderr %q", stderr)
	}
}

func TestNotEnrolledAndUnavailablePassThrough(t *testing.T) {
	for _, r := range []core.Result{
		domain.Fail(core.ExitNotEnrolled, core.CodeNotEnrolled, "not enrolled", "crosshall run -- <agent>"),
		domain.Fail(core.ExitUnavailable, core.CodeOutcomeUnknown, "no answer", "retry with --key k"),
	} {
		ft := &fakeTransport{result: r}
		if _, _, code := run(t, Env{Transport: ft}, "who"); code != r.Status {
			t.Errorf("exit %d, want %d", code, r.Status)
		}
	}
}

func TestJoinPlainShowsHintAndRoster(t *testing.T) {
	members := []domain.Member{
		{Alias: "form", Vendor: "claude", Focus: "payment form", State: "unknown", LastCommandAgoS: 12, Route: "pull", Self: true},
		{Alias: "lint", Vendor: "route", State: "unknown", LastCommandAgoS: 540, Route: "pull"},
	}
	ft := &fakeTransport{result: okResult(t, domain.JoinResult{
		Channel: "checkout", Topic: "payment screen", Alias: "form",
		Hint: domain.Hint("form", "checkout"), Members: members, Omitted: 3,
	})}
	stdout, _, _ := run(t, Env{Transport: ft}, "join", "checkout")
	for _, want := range []string{
		"Joined #checkout as @form",
		"#checkout - payment screen",
		"@form (you)", "payment form", "last command 12s ago",
		"focus unset", "last command 9m ago",
		"3 more: crosshall who",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in\n%s", want, stdout)
		}
	}
}

func TestMissingPartsAreReported(t *testing.T) {
	for _, args := range [][]string{{"who"}, {"whoami"}, {"run", "--", "sh"}, {"owner", "start"}} {
		if _, stderr, code := run(t, Env{}, args...); code != core.ExitError || !strings.Contains(stderr, "not available") {
			t.Errorf("%v: exit %d stderr %q", args, code, stderr)
		}
	}
}

func TestLocalFailuresAreJSONInJSONMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  Env
		args []string
		exit core.ExitCode
		code string
	}{
		{"usage", Env{Transport: &fakeTransport{}}, []string{"--json", "join", "Bad_Name"}, core.ExitUsage, "usage"},
		{"flag error", Env{}, []string{"--json", "--key"}, core.ExitUsage, "usage"},
		{"unknown verb", Env{}, []string{"frobnicate", "--json"}, core.ExitUsage, "usage"},
		{"startup", Env{Start: func(context.Context) error { return errors.New("permission denied") }},
			[]string{"--json", "owner", "start"}, core.ExitError, "local_error"},
		{"transport", Env{Transport: errTransport{}}, []string{"--json", "who"}, core.ExitError, "local_error"},
		{"unavailable", Env{Unavailable: "development build requires CROSSHALL_STATE_DIR"},
			[]string{"--json", "who"}, core.ExitError, "local_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := run(t, tc.env, tc.args...)
			if code != tc.exit {
				t.Fatalf("exit %d, want %d", code, tc.exit)
			}
			if stderr != "" {
				t.Fatalf("stderr in JSON mode: %q", stderr)
			}
			dec := json.NewDecoder(strings.NewReader(stdout))
			var got map[string]any
			if err := dec.Decode(&got); err != nil || dec.More() {
				t.Fatalf("not exactly one JSON object: %q", stdout)
			}
			if got["v"] != float64(1) || got["ok"] != false || got["code"] != tc.code || got["message"] == "" {
				t.Fatalf("object %v", got)
			}
		})
	}
}

type errTransport struct{}

func (errTransport) Do(context.Context, Call) (core.Result, error) {
	return core.Result{}, errors.New("spool directory is not private")
}

func TestJoinPlainShowsRosterChanges(t *testing.T) {
	ft := &fakeTransport{result: okResult(t, domain.JoinResult{
		Channel: "checkout", Alias: "form", Restored: true,
		Changes: []domain.Member{
			{Alias: "tax", Vendor: "codex", Change: "joined"},
			{Alias: "lint", Change: "departed"},
		},
	})}
	stdout, _, _ := run(t, Env{Transport: ft}, "join", "checkout")
	for _, want := range []string{"Already in #checkout as @form", "@tax joined (codex, focus unset)", "@lint departed"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("missing %q in\n%s", want, stdout)
		}
	}
}

func TestJoinCanAskForTheRulesAgain(t *testing.T) {
	ft := &fakeTransport{result: okResult(t, domain.JoinResult{Channel: "checkout", Alias: "form", Restored: true})}
	if _, _, code := run(t, Env{Transport: ft}, "join", "checkout", "--rules"); code != core.ExitOK || len(ft.calls) != 1 {
		t.Fatalf("exit %d, calls %d", code, len(ft.calls))
	}
	if cmd, ok := ft.calls[0].Command.(domain.Join); !ok || !cmd.Rules || cmd.Channel != "checkout" {
		t.Fatalf("command %+v", ft.calls[0].Command)
	}
}

func TestWhoamiRejectsUnrecognizedDisclosureFlags(t *testing.T) {
	for _, args := range [][]string{{"whoami", "full"}, {"whoami", "--full", "--full"}, {"whoami", "--full", "extra"}} {
		if _, _, code := run(t, Env{}, args...); code != core.ExitUsage {
			t.Fatalf("%v: expected usage error, got %d", args, code)
		}
	}
}
