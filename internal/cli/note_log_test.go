package cli

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
)

func TestNoteLogLeaveFocusParsing(t *testing.T) {
	ft := &fakeTransport{result: okResult(t, domain.NoteResult{ID: 1})}
	env := Env{Transport: ft, Stdin: strings.NewReader("from stdin\n")}
	for _, tc := range []struct {
		args  []string
		want  domain.Command
		owned bool // carries an operation key
	}{
		{[]string{"note", "@tax", "@a11y", "@tax, heads up"}, domain.Note{To: []string{"tax", "a11y"}, Body: "@tax, heads up"}, true},
		{[]string{"note", "--all", "-"}, domain.Note{All: true, Body: "from stdin\n"}, true},
		{[]string{"note", "@tax", "--", "--all is text"}, domain.Note{To: []string{"tax"}, Body: "--all is text"}, true},
		{[]string{"log"}, domain.Log{}, false},
		{[]string{"log", "--since", "#12", "--limit=5"}, domain.Log{Since: ptr(12), Limit: 5}, false},
		{[]string{"log", "--since", "0"}, domain.Log{Since: ptr(0)}, false},
		{[]string{"leave"}, domain.Leave{}, true},
		{[]string{"focus", "refund rounding"}, domain.Focus{Text: "refund rounding"}, true},
	} {
		ft.calls = nil
		if _, stderr, code := run(t, env, tc.args...); code != core.ExitOK {
			t.Fatalf("%v: exit %d %s", tc.args, code, stderr)
		}
		call := ft.calls[0]
		if !reflect.DeepEqual(call.Command, tc.want) || (call.OpKey != "") != tc.owned {
			t.Errorf("%v: got %+v key %q", tc.args, call.Command, call.OpKey)
		}
	}
}

func ptr(n int64) *int64 { return &n }

func TestNoteLogUsageErrorsSendNothing(t *testing.T) {
	ft := &fakeTransport{}
	for _, args := range [][]string{
		{"note", "hello"},
		{"note", "@tax"},
		{"note", "@tax", "--all", "x"},
		{"note", "@tax", "--due", "5m", "x"},
		{"note", "@tax", "one", "two"},
		{"log", "--since", "-1"},
		{"log", "--limit", "0"},
		{"log", "--limit", "201"},
		{"log", "extra"},
		{"leave", "now"},
		{"focus"},
		{"focus", "a", "b"},
	} {
		if _, _, code := run(t, Env{Transport: ft}, args...); code != core.ExitUsage {
			t.Errorf("%v: exit %d", args, code)
		}
	}
	if len(ft.calls) != 0 {
		t.Fatalf("usage errors reached the transport: %+v", ft.calls)
	}
}

func TestNoteLogLeaveFocusPlainOutput(t *testing.T) {
	defer func(f func() time.Time) { now = f }(now)
	base := time.Date(2026, 11, 18, 13, 0, 0, 0, time.Local)
	now = func() time.Time { return base }
	at := base.UnixMilli()
	due := base.Add(30 * time.Minute).UnixMilli()
	ask, x := int64(41), int64(3)

	for _, tc := range []struct {
		args []string
		data any
		want []string
	}{
		{[]string{"note", "--all", "fyi"}, domain.NoteResult{ID: 40, Recipients: []domain.Delivery{
			{Alias: "a11y", State: "stored", Next: "waits for its next crosshall command"},
			{Alias: "tax", State: "picked_up", PickedUpAtMS: at},
		}}, []string{"#40 note stored; nobody owes an answer.", "@a11y  stored -> waits", "@tax   picked up 13:00"}},
		{[]string{"log", "--limit", "2"}, domain.LogResult{Channel: "checkout", Earlier: 39, Next: ptr(37), Messages: []domain.LogEntry{
			{ID: 41, Kind: "ask", Sender: "form", To: []string{"a11y", "tax"}, Exchange: &x, DueAtMS: &due, CreatedAtMS: at, Body: "review?\nline two\n"},
			{ID: 42, Kind: "reply", Outcome: "decline", Sender: "tax", To: []string{"form"}, InReplyTo: &ask, CreatedAtMS: at, Body: "no"},
		}}, []string{
			"#41 13:00 ask from @form to @a11y @tax, due 13:30\n  review?\n  line two\n",
			"#42 13:00 reply (decline) from @tax to @form, re #41\n  no\n",
			"... 39 earlier: crosshall log --since 37 --limit 2\n",
		}},
		{[]string{"log"}, domain.LogResult{Channel: "checkout", Later: 5, Next: ptr(20), Messages: []domain.LogEntry{
			{ID: 20, Kind: "note", Sender: "form", All: true, CreatedAtMS: at, Body: "fyi"},
		}}, []string{"#20 13:00 note from @form to all\n", "... 5 later: crosshall log --since 20\n"}},
		{[]string{"log"}, domain.LogResult{Channel: "checkout", Messages: []domain.LogEntry{}}, []string{"#checkout: no messages.\n"}},
		{[]string{"leave"}, domain.LeaveResult{Channel: "checkout", Alias: "tax", Owed: 1},
			[]string{"Left #checkout as @tax. 1 request(s) you owe stay open and visible to the owner.\nTo come back: crosshall join checkout\n"}},
		{[]string{"focus", "x"}, domain.FocusResult{Alias: "tax", Focus: "x", Changed: true}, []string{"@tax focus: x\n"}},
	} {
		ft := &fakeTransport{result: okResult(t, tc.data)}
		out, stderr, code := run(t, Env{Transport: ft}, tc.args...)
		if code != core.ExitOK {
			t.Fatalf("%v: exit %d %s", tc.args, code, stderr)
		}
		for _, want := range tc.want {
			if !strings.Contains(out, want) {
				t.Errorf("%v: missing %q in\n%s", tc.args, want, out)
			}
		}
	}
}
