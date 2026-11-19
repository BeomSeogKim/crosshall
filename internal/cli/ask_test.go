package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
)

func TestAskParsing(t *testing.T) {
	ft := &fakeTransport{result: okResult(t, domain.AskResult{ID: 42})}
	_, stderr, code := run(t, Env{Transport: ft}, "ask", "@tax", "@a11y", "review src/pay/refund.go:40-88", "--due", "45m", "--re", "#7")
	if code != core.ExitOK {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	want := domain.Ask{To: []string{"tax", "a11y"}, Body: "review src/pay/refund.go:40-88", Due: "45m", Re: 7}
	if got := ft.calls[0].Command.(domain.Ask); !reflect.DeepEqual(got, want) || ft.calls[0].OpKey == "" {
		t.Fatalf("got %+v key %q", got, ft.calls[0].OpKey)
	}

	ft.calls = nil
	run(t, Env{Transport: ft}, "ask", "--all", "--each", "--new", "status?")
	if got := ft.calls[0].Command.(domain.Ask); !got.All || !got.Each || !got.New || got.Body != "status?" {
		t.Fatalf("got %+v", got)
	}
}

func TestAskBodySources(t *testing.T) {
	ft := &fakeTransport{result: okResult(t, domain.AskResult{ID: 1})}
	env := Env{Transport: ft, Stdin: strings.NewReader("from stdin\nsecond line\n")}
	run(t, env, "ask", "@tax", "-")
	if got := ft.calls[0].Command.(domain.Ask).Body; got != "from stdin\nsecond line\n" {
		t.Fatalf("stdin body %q", got)
	}
	path := filepath.Join(t.TempDir(), "q.txt")
	if err := os.WriteFile(path, []byte("from a file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	run(t, env, "ask", "@tax", "--file", path)
	if got := ft.calls[1].Command.(domain.Ask).Body; got != "from a file\n" {
		t.Fatalf("file body %q", got)
	}
	big := filepath.Join(t.TempDir(), "big.txt")
	if err := os.WriteFile(big, make([]byte, domain.MaxBody+1), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, code := run(t, env, "ask", "@tax", "--file", big); code != core.ExitUsage {
		t.Fatalf("oversized body: exit %d", code)
	}
}

func TestAskAndReplyUsageErrors(t *testing.T) {
	ft := &fakeTransport{}
	for _, args := range [][]string{
		{"ask", "no target"},
		{"ask", "@tax"},
		{"ask", "@tax", "one", "two"},
		{"ask", "@tax", "x", "-"},
		{"ask", "@Bad_Alias", "x"},
		{"ask", "@tax", "x", "--due", "soon"},
		{"ask", "@tax", "x", "--due", "10s"},
		{"ask", "@tax", "x", "--re", "abc"},
		{"ask", "@tax", "x", "--bogus"},
		{"ask", "@tax", "--file", "/nonexistent/crosshall-test"},
		{"reply"},
		{"reply", "42"},
		{"reply", "x42", "ok"},
		{"reply", "42", "ok", "--decline", "--more-time"},
		{"reply", "42", "a", "b"},
	} {
		if _, _, code := run(t, Env{Transport: ft}, args...); code != core.ExitUsage {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if len(ft.calls) != 0 {
		t.Fatalf("usage errors reached the transport: %+v", ft.calls)
	}
}

func TestReplyParsing(t *testing.T) {
	ft := &fakeTransport{result: okResult(t, domain.ReplyResult{ID: 45, Ask: 42})}
	run(t, Env{Transport: ft}, "reply", "#42", "looks right")
	run(t, Env{Transport: ft}, "reply", "42", "--decline")
	run(t, Env{Transport: ft}, "reply", "42", "--more-time", "need an hour")
	want := []domain.Reply{
		{To: 42, Body: "looks right"},
		{To: 42, Outcome: domain.OutcomeDecline},
		{To: 42, Body: "need an hour", Outcome: domain.OutcomeMoreTime},
	}
	for i, w := range want {
		if got := ft.calls[i].Command.(domain.Reply); got != w || ft.calls[i].OpKey == "" {
			t.Errorf("call %d: %+v", i, got)
		}
	}
}

func TestAskAndReplyPlainOutput(t *testing.T) {
	defer func(f func() time.Time) { now = f }(now)
	base := time.Date(2026, 11, 18, 13, 0, 0, 0, time.Local)
	now = func() time.Time { return base }
	due := base.Add(30 * time.Minute).UnixMilli()

	ft := &fakeTransport{result: okResult(t, domain.AskResult{
		ID: 42, Exchange: 7, NewExchange: true, DueAtMS: due, Debit: 2, Remaining: 6,
		Recipients: []domain.Delivery{
			{Alias: "a11y", State: "stored", Next: "waits for its next crosshall command"},
			{Alias: "tax", State: "picked_up", PickedUpAtMS: base.UnixMilli()},
		},
	})}
	out, _, _ := run(t, Env{Transport: ft}, "ask", "@tax", "@a11y", "review")
	for _, want := range []string{"#42 stored. due 13:30. exchange x7 (new): 6 left.", "@a11y  stored -> waits", "@tax   picked up 13:00"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}

	for _, tc := range []struct {
		r    domain.ReplyResult
		want string
	}{
		{domain.ReplyResult{ID: 45, Ask: 42, To: "form", Outcome: "answer"}, "#45 answers #42 (to @form).\n"},
		{domain.ReplyResult{ID: 45, Ask: 42, To: "form", Outcome: "answer", LateByMS: 300000}, "#45 answers #42 (to @form). Late by 5m.\n"},
		{domain.ReplyResult{ID: 46, Ask: 42, To: "form", Outcome: "decline"}, "#46 declines #42 (to @form).\n"},
		{domain.ReplyResult{ID: 47, Ask: 42, To: "form", Outcome: "more_time", DueAtMS: due}, "#47 asks for more time on #42 (to @form); it is still due 13:30.\n"},
	} {
		ft := &fakeTransport{result: okResult(t, tc.r)}
		if out, _, _ := run(t, Env{Transport: ft}, "reply", "42", "x"); out != tc.want {
			t.Errorf("got %q, want %q", out, tc.want)
		}
	}
}

func TestAskJSONIsOneObject(t *testing.T) {
	ft := &fakeTransport{result: okResult(t, domain.AskResult{ID: 42, Exchange: 7})}
	out, _, code := run(t, Env{Transport: ft}, "--json", "ask", "@tax", "x")
	dec := json.NewDecoder(strings.NewReader(out))
	var got map[string]any
	if code != core.ExitOK || dec.Decode(&got) != nil || dec.More() || got["kind"] != "ask" || got["id"] != float64(42) {
		t.Fatalf("exit %d output %q", code, out)
	}
}

// Finding: the text is sent byte for byte, so different bytes never replay as
// one send, and invalid UTF-8 is refused before it is encoded.
func TestBodyBytesAreKept(t *testing.T) {
	dir := t.TempDir()
	one, two := filepath.Join(dir, "one"), filepath.Join(dir, "two")
	if err := os.WriteFile(one, []byte("sample\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(two, []byte("sample\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ft := &fakeTransport{result: okResult(t, domain.AskResult{ID: 1})}
	env := Env{Transport: ft, Stdin: strings.NewReader("sample\n")}
	run(t, env, "ask", "@tax", "--file", one)
	run(t, env, "ask", "@tax", "--file", two)
	run(t, env, "ask", "@tax", "-")
	b := func(i int) string { return ft.calls[i].Command.(domain.Ask).Body }
	if b(0) != "sample\n" || b(1) != "sample\n\n" || b(2) != b(0) {
		t.Fatalf("bodies %q %q %q", b(0), b(1), b(2))
	}
	bad := filepath.Join(dir, "bad")
	if err := os.WriteFile(bad, []byte("ok\xff"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"ask", "@tax", "--file", bad}, {"ask", "@tax", "ok\xfe"}, {"reply", "1", "ok\xff"}} {
		if _, _, code := run(t, Env{Transport: ft}, args...); code != core.ExitUsage {
			t.Errorf("%q: exit %d, want 2", args, code)
		}
	}
	if len(ft.calls) != 3 {
		t.Fatalf("invalid text reached the transport: %d calls", len(ft.calls))
	}
}

// Finding: only a whole, valid @alias is a recipient; other text starting
// with @ is the message, and -- makes the next argument the text.
func TestAliasInsideTheText(t *testing.T) {
	ft := &fakeTransport{result: okResult(t, domain.AskResult{ID: 1})}
	run(t, Env{Transport: ft}, "ask", "@tax", "@tax, is this ready?")
	run(t, Env{Transport: ft}, "ask", "@tax", "--", "@lint")
	run(t, Env{Transport: ft}, "ask", "@tax", "--", "--due is in the text")
	want := []domain.Ask{
		{To: []string{"tax"}, Body: "@tax, is this ready?"},
		{To: []string{"tax"}, Body: "@lint"},
		{To: []string{"tax"}, Body: "--due is in the text"},
	}
	if len(ft.calls) != len(want) {
		t.Fatalf("%d calls", len(ft.calls))
	}
	for i, w := range want {
		if got := ft.calls[i].Command.(domain.Ask); !reflect.DeepEqual(got, w) {
			t.Errorf("call %d: %+v", i, got)
		}
	}
	if _, _, code := run(t, Env{Transport: ft}, "ask", "@tax", "--", "a", "b"); code != core.ExitUsage {
		t.Fatalf("two arguments after --: exit %d", code)
	}
}

// Finding: a refused fan-out keeps its list of bad recipients in --json.
func TestRefusalKeepsInvalidTargetsInJSON(t *testing.T) {
	data, _ := json.Marshal(map[string]any{
		"message": "nothing was sent", "hint": "crosshall who",
		"invalid_targets": []map[string]string{{"alias": "nobody", "reason": "unknown"}},
	})
	ft := &fakeTransport{result: core.Result{Status: core.ExitRefused, Code: core.CodeUnknownAlias, Data: data}}
	out, _, code := run(t, Env{Transport: ft}, "--json", "ask", "@nobody", "x")
	var got map[string]any
	if code != core.ExitRefused || json.Unmarshal([]byte(out), &got) != nil {
		t.Fatalf("exit %d out %q", code, out)
	}
	if list, _ := got["invalid_targets"].([]any); len(list) != 1 || got["code"] != "unknown_alias" || got["ok"] != false {
		t.Fatalf("refusal %v", got)
	}
}

func TestReplyPlainShowsDelivery(t *testing.T) {
	ft := &fakeTransport{result: okResult(t, domain.ReplyResult{ID: 45, Ask: 42, To: "form", Outcome: "answer",
		Delivery: domain.Delivery{Alias: "form", State: "stored", Next: "waits for its next crosshall command"}})}
	out, _, _ := run(t, Env{Transport: ft}, "reply", "42", "x")
	if !strings.Contains(out, "@form  stored -> waits for its next crosshall command") {
		t.Fatalf("output %q", out)
	}
}

// An ask that spends an exchange's last unit says so. It describes its own
// debit only: an owner grant can add units later, and a replay is frozen.
func TestAskSaysWhenItSpentTheLastUnit(t *testing.T) {
	defer func(f func() time.Time) { now = f }(now)
	base := time.Date(2026, 11, 18, 13, 0, 0, 0, time.Local)
	now = func() time.Time { return base }
	due := base.Add(30 * time.Minute).UnixMilli()
	for _, tc := range []struct {
		remaining int64
		want      string
	}{
		{1, "#42 stored. due 13:30. exchange x7: 1 left.\n"},
		{0, "#42 stored. due 13:30. exchange x7: 0 left; this request spent its last unit.\n"},
	} {
		ft := &fakeTransport{result: okResult(t, domain.AskResult{ID: 42, Exchange: 7, DueAtMS: due, Debit: 1, Remaining: tc.remaining})}
		out, _, _ := run(t, Env{Transport: ft}, "ask", "@tax", "review")
		if !strings.HasPrefix(out, tc.want) {
			t.Errorf("remaining %d: got %q, want prefix %q", tc.remaining, out, tc.want)
		}
	}
}
