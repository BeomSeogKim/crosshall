package cli

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
)

func TestVersionFlagPrintsTheVersion(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"--version"}} {
		out, stderr, code := run(t, Env{Version: "v9.9.9"}, args...)
		if code != core.ExitOK || out != "v9.9.9\n" {
			t.Errorf("%v: exit %d stdout %q stderr %q", args, code, out, stderr)
		}
	}
	if _, _, code := run(t, Env{Version: "v9.9.9"}, "--version", "extra"); code != core.ExitUsage {
		t.Errorf("--version with an argument: exit %d", code)
	}
}

func TestOwnerLogTakesTheMemberPagingFlags(t *testing.T) {
	ft := &fakeTransport{result: okResult(t, domain.LogResult{Channel: "checkout", Messages: []domain.LogEntry{}})}
	env := Env{Transport: ft, OwnerGate: func() string { return "" }}
	for _, tc := range []struct {
		args []string
		want domain.OwnerLog
	}{
		{[]string{"owner", "log", "checkout"}, domain.OwnerLog{Channel: "checkout"}},
		{[]string{"owner", "log", "#checkout", "--since", "#12", "--limit=5"}, domain.OwnerLog{Channel: "checkout", Since: ptr(12), Limit: 5}},
		{[]string{"owner", "log", "checkout", "--since", "0"}, domain.OwnerLog{Channel: "checkout", Since: ptr(0)}},
	} {
		ft.calls = nil
		if _, stderr, code := run(t, env, tc.args...); code != core.ExitOK {
			t.Fatalf("%v: exit %d %s", tc.args, code, stderr)
		}
		if call := ft.calls[0]; !reflect.DeepEqual(call.Command, tc.want) || call.OpKey != "" || !call.Owner {
			t.Errorf("%v: got %+v", tc.args, call)
		}
	}
	ft.calls = nil
	for _, args := range [][]string{
		{"owner", "log"},
		{"owner", "log", "checkout", "--since", "-1"},
		{"owner", "log", "checkout", "--limit", "0"},
		{"owner", "log", "checkout", "--limit", "201"},
		{"owner", "log", "checkout", "extra"},
	} {
		if _, _, code := run(t, env, args...); code != core.ExitUsage {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	if len(ft.calls) != 0 {
		t.Fatalf("usage errors reached the transport: %+v", ft.calls)
	}
}

func TestOwnerLogContinuationNamesTheOwnerCommand(t *testing.T) {
	at := time.Now().UnixMilli()
	for _, tc := range []struct {
		args []string
		r    domain.LogResult
		want string
	}{
		{[]string{"owner", "log", "checkout", "--limit", "2"}, domain.LogResult{Channel: "checkout", Earlier: 39, Next: ptr(0)},
			"... 39 earlier: crosshall owner log checkout --since 0 --limit 2\n"},
		{[]string{"owner", "log", "checkout", "--since", "0"}, domain.LogResult{Channel: "checkout", Later: 5, Next: ptr(20)},
			"... 5 later: crosshall owner log checkout --since 20\n"},
	} {
		tc.r.Messages = []domain.LogEntry{{ID: 20, Kind: "note", Sender: "form", All: true, CreatedAtMS: at, Body: "fyi"}}
		ft := &fakeTransport{result: okResult(t, tc.r)}
		out, stderr, code := run(t, Env{Transport: ft, OwnerGate: func() string { return "" }}, tc.args...)
		if code != core.ExitOK || !strings.HasSuffix(out, tc.want) || strings.Contains(out, "crosshall log ") {
			t.Errorf("%v: exit %d stderr %q, want suffix %q in\n%s", tc.args, code, stderr, tc.want, out)
		}
	}
}

func TestRosterWithoutAdapterDoesNotReadAsAnError(t *testing.T) {
	ft := &fakeTransport{result: okResult(t, domain.WhoResult{Channel: "checkout", Members: []domain.Member{
		{Alias: "form", Vendor: "claude", State: "unknown", LastCommandAgoS: 12, Route: "pull", Self: true},
		{Alias: "lint", Vendor: "route", State: "unknown", LastCommandAgoS: 540, Route: "pull"},
	}})}
	out, _, code := run(t, Env{Transport: ft}, "who")
	if code != core.ExitOK || strings.Contains(out, "unknown") {
		t.Fatalf("exit %d, the roster still says unknown:\n%s", code, out)
	}
	for _, want := range []string{"last command 12s ago", "last command 9m ago", "no vendor adapter"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Count(out, "no vendor adapter") != 1 {
		t.Errorf("the explanation should appear once:\n%s", out)
	}
	ft.result = okResult(t, domain.WhoResult{Channel: "checkout", Members: []domain.Member{
		{Alias: "form", Vendor: "claude", State: "working", LastCommandAgoS: 12, Route: "pull", Self: true},
	}})
	if out, _, _ := run(t, Env{Transport: ft}, "who"); !strings.Contains(out, "working, last command 12s ago") || strings.Contains(out, "adapter") {
		t.Errorf("a reported state is shown as is:\n%s", out)
	}
}

func TestDoctorSaysANeverStartedHallIsNotStarted(t *testing.T) {
	data, _ := json.Marshal(map[string]any{
		"checks": []doctorCheck{
			{Name: "lock", Detail: "not started yet"}, {Name: "readiness", Detail: "not started yet"},
			{Name: "test_clock", OK: true, Detail: "release builds ignore X"},
		},
		"hint": "crosshall owner start", "not_started": true,
	})
	env := Env{OwnerGate: func() string { return "" }, Doctor: func(context.Context) core.Result { return core.Result{Status: core.ExitError, Data: data} }}
	out, _, _ := run(t, env, "owner", "doctor")
	if strings.Contains(out, "FAIL") || strings.Contains(out, "not running") ||
		!strings.Contains(out, "Not started yet") || !strings.Contains(out, "Start it with crosshall owner start;") || !strings.Contains(out, "crosshall wait and crosshall owner doctor do not") ||
		!strings.Contains(out, "PASS test_clock") {
		t.Fatalf("a never-started hall should read as not started, with no FAIL lines:\n%s", out)
	}
	out, _, _ = run(t, env, "--json", "owner", "doctor")
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["not_started"] != true || got["hint"] != "crosshall owner start" {
		t.Fatalf("JSON: %v %s", err, out)
	}
}

func TestDoctorNamesTheStartCommand(t *testing.T) {
	data, _ := json.Marshal(map[string]any{
		"checks": []doctorCheck{{Name: "lock", Detail: "dispatcher lock is absent or idle"}, {Name: "readiness", Detail: "no live dispatcher readiness marker"}},
		"hint":   "crosshall owner start",
	})
	env := Env{OwnerGate: func() string { return "" }, Doctor: func(context.Context) core.Result { return core.Result{Status: core.ExitError, Data: data} }}
	out, _, code := run(t, env, "owner", "doctor")
	if code != core.ExitError || !strings.Contains(out, "next: crosshall owner start") {
		t.Fatalf("exit %d, plain output has no start hint:\n%s", code, out)
	}
	out, _, _ = run(t, env, "--json", "owner", "doctor")
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["hint"] != "crosshall owner start" {
		t.Fatalf("JSON hint: %v %s", err, out)
	}
	data, _ = json.Marshal(map[string]any{"checks": []doctorCheck{{Name: "lock", OK: true, Detail: "dispatcher lock held"}}})
	if out, _, _ := run(t, env, "owner", "doctor"); strings.Contains(out, "next:") {
		t.Fatalf("a passing doctor names no next command:\n%s", out)
	}
}
