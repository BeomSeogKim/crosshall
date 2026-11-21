package cli

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
)

func TestLedgerCommandsUseExpectedIngressAndKeys(t *testing.T) {
	disabled, rate, ceiling := int64(0), int64(7), int64(5)
	for _, tc := range []struct {
		args []string
		call Call
	}{
		{[]string{"read", "#7", "5"}, Call{Command: domain.Read{IDs: []int64{7, 5}}, OpKey: "test-key"}},
		{[]string{"inbox"}, Call{Command: domain.Inbox{}}},
		{[]string{"waiting"}, Call{Command: domain.Waiting{}}},
		{[]string{"status"}, Call{Command: domain.Status{}}},
		{[]string{"owner", "status", "--all"}, Call{Owner: true, Command: domain.OwnerStatus{}}},
		{[]string{"owner", "status", "--channel=checkout"}, Call{Owner: true, Command: domain.OwnerStatus{Channel: "checkout"}}},
		{[]string{"owner", "cancel", "checkout/#7"}, Call{Owner: true, Command: domain.Cancel{Channel: "checkout", ID: 7}, OpKey: "test-key"}},
		{[]string{"owner", "budget", "policy", "#checkout"}, Call{Owner: true, Command: domain.BudgetPolicy{Channel: "checkout"}}},
		{[]string{"owner", "budget", "policy", "checkout/@tax"}, Call{Owner: true, Command: domain.BudgetPolicy{Channel: "checkout", Alias: "tax"}}},
		{[]string{"owner", "budget", "policy", "checkout", "--refill", "0"}, Call{Owner: true, Command: domain.BudgetPolicy{Channel: "checkout", Refill: &disabled}, OpKey: "test-key"}},
		{[]string{"owner", "budget", "policy", "checkout/@tax", "--ceiling", "0"}, Call{Owner: true, Command: domain.BudgetPolicy{Channel: "checkout", Alias: "tax", Ceiling: &disabled}, OpKey: "test-key"}},
		{[]string{"owner", "budget", "policy", "checkout/@tax", "--ceiling=5", "--refill", "7/h"}, Call{Owner: true, Command: domain.BudgetPolicy{Channel: "checkout", Alias: "tax", Refill: &rate, Ceiling: &ceiling}, OpKey: "test-key"}},
		{[]string{"owner", "budget", "grant", "checkout/x3", "2"}, Call{Owner: true, Command: domain.BudgetGrant{Channel: "checkout", ExchangeID: 3, Units: 2}, OpKey: "test-key"}},
		{[]string{"owner", "budget", "grant", "checkout/@tax", "--roots", "1"}, Call{Owner: true, Command: domain.BudgetGrant{Channel: "checkout", Alias: "tax", Roots: 1}, OpKey: "test-key"}},
		{[]string{"owner", "budget", "grant", "checkout", "--pool", "3"}, Call{Owner: true, Command: domain.BudgetGrant{Channel: "checkout", Pool: 3}, OpKey: "test-key"}},
	} {
		t.Run(strings.Join(tc.args, " "), func(t *testing.T) {
			ft := &fakeTransport{result: okResult(t, struct{}{})}
			_, stderr, code := run(t, Env{Transport: ft, NewKey: func() string { return "test-key" }}, append([]string{"--json"}, tc.args...)...)
			if code != core.ExitOK || !reflect.DeepEqual(ft.calls, []Call{tc.call}) {
				t.Fatalf("exit=%d, stderr=%q, calls=%+v", code, stderr, ft.calls)
			}
		})
	}
}

func TestBudgetPolicyRejectsInvalidArgumentsLocally(t *testing.T) {
	for _, args := range [][]string{
		{},
		{"checkout/"},
		{"checkout/x1"},
		{"checkout/@tax/other"},
		{"checkout", "--refill", "3"},
		{"checkout", "--refill", "-1/h"},
		{"checkout", "--refill", "1.5/h"},
		{"checkout", "--refill"},
		{"checkout", "--refill", "0", "--refill", "3/h"},
		{"checkout", "--ceiling", "-1"},
		{"checkout", "--ceiling", "5", "--ceiling", "6"},
		{"checkout", "--ceiling", "9223372036854775808"},
		{"checkout", "--pool", "5"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			ft := &fakeTransport{}
			command := append([]string{"owner", "budget", "policy"}, args...)
			_, stderr, code := run(t, Env{Transport: ft}, command...)
			if code != core.ExitUsage || !strings.Contains(stderr, "usage: crosshall owner budget policy") || len(ft.calls) != 0 {
				t.Fatalf("exit=%d stderr=%q calls=%+v", code, stderr, ft.calls)
			}
		})
	}
}

func TestBudgetPolicyPrintsEffectiveSettings(t *testing.T) {
	for _, tc := range []struct {
		target, alias string
		refill        int64
		want          string
	}{
		{"checkout", "", 3, "#checkout: root token refill 3/h, automatic ceiling 5.\n"},
		{"checkout/@tax", "tax", 0, "#checkout/@tax: root token refill 0/h, automatic ceiling 5.\n"},
	} {
		ft := &fakeTransport{result: okResult(t, domain.BudgetPolicyResult{
			Channel: "checkout", Alias: tc.alias, Refill: tc.refill, Ceiling: 5,
		})}
		out, stderr, code := run(t, Env{Transport: ft}, "owner", "budget", "policy", tc.target)
		if code != core.ExitOK || stderr != "" || out != tc.want {
			t.Fatalf("exit=%d stdout=%q stderr=%q want=%q", code, out, stderr, tc.want)
		}
	}
}

func TestStatusShowsNextRootTokenWhenScheduled(t *testing.T) {
	defer func(f func() time.Time) { now = f }(now)
	base := time.Date(2026, 11, 18, 23, 50, 0, 0, time.Local)
	now = func() time.Time { return base }
	for _, tc := range []struct {
		next int64
		want string
	}{
		{0, ""},
		{base.Add(20 * time.Minute).UnixMilli(), "Next root token at Nov 19 00:10.\n"},
	} {
		ft := &fakeTransport{result: okResult(t, domain.StatusResult{
			Self: &domain.StatusSelf{Alias: "tax", NextRootTokenAtMS: tc.next},
		})}
		out, stderr, code := run(t, Env{Transport: ft}, "status")
		want := "No channels.\nYou (@tax): 0 root token(s) to open a new exchange; taking part in no exchange.\n" + tc.want
		if code != core.ExitOK || stderr != "" || out != want {
			t.Fatalf("exit=%d stdout=%q stderr=%q want=%q", code, out, stderr, want)
		}
		out, stderr, code = run(t, Env{Transport: ft}, "--json", "status")
		var body struct {
			Self map[string]json.RawMessage
		}
		if err := json.Unmarshal([]byte(out), &body); err != nil || code != core.ExitOK || stderr != "" {
			t.Fatalf("exit=%d stdout=%q stderr=%q err=%v", code, out, stderr, err)
		}
		if next, present := body.Self["next_root_token_at_ms"]; tc.next == 0 && present ||
			tc.next > 0 && string(next) != strconv.FormatInt(tc.next, 10) {
			t.Fatalf("wrong next_root_token_at_ms: %s", next)
		}
	}
}

func TestBudgetGrantPrintsUnitOrUnits(t *testing.T) {
	for _, tc := range []struct {
		arg   string
		units int64
		want  string
	}{
		{"1", 1, "#checkout/x3: added 1 unit, 5 remaining.\n"},
		{"2", 2, "#checkout/x3: added 2 units, 5 remaining.\n"},
	} {
		ft := &fakeTransport{result: okResult(t, domain.BudgetGrantResult{
			Channel: "checkout", ExchangeID: 3, Units: tc.units, Remaining: 5,
		})}
		stdout, stderr, code := run(t, Env{Transport: ft}, "owner", "budget", "grant", "checkout/x3", tc.arg)
		if code != core.ExitOK || stdout != tc.want || stderr != "" {
			t.Fatalf("grant %s: exit=%d stdout=%q stderr=%q want=%q", tc.arg, code, stdout, stderr, tc.want)
		}
	}
}

func TestPendingUsesPlainTrailerOrJSONField(t *testing.T) {
	const id = int64(9007199254740993)
	ft := &fakeTransport{result: okResult(t, struct {
		Pending   []int64 `json:"pending"`
		OwedCount int     `json:"owed_count"`
	}{[]int64{id}, 2})}
	stdout, stderr, code := run(t, Env{Transport: ft}, "inbox")
	if code != core.ExitOK || stderr != "" || strings.Contains(stdout, "Run crosshall inbox") {
		t.Fatalf("plain %d stdout=%q stderr=%q", code, stdout, stderr)
	}
	stdout, stderr, code = run(t, Env{Transport: ft}, "read")
	if code != core.ExitOK || stderr != "" || !strings.HasSuffix(stdout, "[crosshall] 1 unread, 2 owed. Run crosshall read.\n") {
		t.Fatalf("plain %d stdout=%q stderr=%q", code, stdout, stderr)
	}

	stdout, stderr, code = run(t, Env{Transport: ft}, "inbox", "--json")
	var out struct {
		Pending   []int64 `json:"pending"`
		OwedCount int     `json:"owed_count"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); code != core.ExitOK || stderr != "" || err != nil ||
		!reflect.DeepEqual(out.Pending, []int64{id}) || out.OwedCount != 2 || strings.Contains(stdout, "Run crosshall inbox") {
		t.Fatalf("JSON %d stdout=%q stderr=%q decoded=%+v error=%v", code, stdout, stderr, out, err)
	}

	ft.result = core.Result{Status: core.ExitRefused, Code: core.CodeNotFound,
		Data: json.RawMessage(`{"message":"missing","pending":[9007199254740993],"roster_changes":[]}`)}
	stdout, stderr, code = run(t, Env{Transport: ft}, "read", "1", "--json")
	out.Pending = nil
	if err := json.Unmarshal([]byte(stdout), &out); code != core.ExitRefused || stderr != "" || err != nil ||
		!reflect.DeepEqual(out.Pending, []int64{id}) {
		t.Fatalf("refusal %d stdout=%q stderr=%q decoded=%+v error=%v", code, stdout, stderr, out, err)
	}
}

// A cancelled, unread request has its own heading: it is not unread work,
// and an inbox holding only it is not empty.
func TestInboxShowsCancelledRequestsApart(t *testing.T) {
	ft := &fakeTransport{result: okResult(t, domain.InboxResult{
		Unread:    []domain.InboxItem{{ID: 3, Kind: "note", Sender: "form"}},
		Cancelled: []domain.InboxItem{{ID: 2, Kind: "ask", Sender: "form"}},
	})}
	out, _, _ := run(t, Env{Transport: ft}, "inbox")
	want := "Unread\n  #3 note from @form\nCancelled by the owner (no answer owed)\n  #2 ask from @form\n"
	if !strings.HasPrefix(out, want) {
		t.Fatalf("inbox:\n%s\nwant prefix:\n%s", out, want)
	}
	ft = &fakeTransport{result: okResult(t, domain.InboxResult{Cancelled: []domain.InboxItem{{ID: 2, Kind: "ask", Sender: "form"}}})}
	if out, _, _ := run(t, Env{Transport: ft}, "inbox"); strings.Contains(out, "Inbox is empty.") {
		t.Fatalf("an inbox with a cancellation reads as empty:\n%s", out)
	}
}

// Reading a request the owner cancelled says that no answer is owed.
func TestReadOfACancelledRequestSaysNoAnswerIsOwed(t *testing.T) {
	ft := &fakeTransport{result: okResult(t, domain.ReadResult{Messages: []domain.ReadMessage{
		{ID: 2, Kind: "ask", Sender: "form", Channel: "checkout", Body: "check the totals", ObligationState: "cancelled"},
	}})}
	out, _, _ := run(t, Env{Transport: ft}, "read", "2")
	if !strings.Contains(out, "Cancelled by the owner; no answer is owed.\n") || strings.Contains(out, "Reply with") {
		t.Fatalf("read of a cancelled request:\n%s", out)
	}
}

// Every member result ends with the status line, so it can stand for an
// inbox check. It names a command only when something is unread: an owed
// request needs no fetch, and nothing waiting is said in so many words.
func TestStatusLineEndsEveryMemberResult(t *testing.T) {
	trailer := func(pending []int64, owed int) *fakeTransport {
		return &fakeTransport{result: okResult(t, struct {
			Pending   []int64 `json:"pending"`
			OwedCount int     `json:"owed_count"`
		}{pending, owed})}
	}
	for _, verb := range []string{"read", "inbox", "who", "waiting"} {
		if out, _, _ := run(t, Env{Transport: trailer([]int64{}, 0)}, verb); !strings.HasSuffix(out, "\n[crosshall] 0 unread, 0 owed.\n") {
			t.Errorf("%s with nothing waiting:\n%s", verb, out)
		}
	}
	if out, _, _ := run(t, Env{Transport: trailer([]int64{}, 1)}, "read"); !strings.HasSuffix(out, "\n[crosshall] 0 unread, 1 owed.\n") {
		t.Fatalf("an owed request alone names a command:\n%s", out)
	}
	if out, _, _ := run(t, Env{Transport: trailer([]int64{4, 9}, 1)}, "read"); !strings.HasSuffix(out, "\n[crosshall] 2 unread, 1 owed. Run crosshall read.\n") {
		t.Fatalf("unread work does not name the pickup:\n%s", out)
	}
	left := trailer([]int64{4}, 0)
	if out, _, _ := run(t, Env{Transport: left}, "leave"); !strings.HasSuffix(out, "[crosshall] 1 unread, 0 owed. After rejoining, run crosshall inbox.\n") {
		t.Fatalf("a member that left with work waiting:\n%s", out)
	}
	if out, _, _ := run(t, Env{Transport: trailer([]int64{}, 0)}, "leave"); strings.Contains(out, "unread") {
		t.Fatalf("a member that left with nothing waiting:\n%s", out)
	}
	refusal := &fakeTransport{result: core.Result{Status: core.ExitRefused, Code: core.CodeCancelled,
		Data: json.RawMessage(`{"message":"the owner cancelled #2","hint":"crosshall inbox","pending":[],"owed_count":0}`)}}
	if _, stderr, _ := run(t, Env{Transport: refusal}, "reply", "2", "x"); !strings.HasSuffix(stderr, "next: crosshall inbox\n[crosshall] 0 unread, 0 owed.\n") {
		t.Fatalf("a refusal lost its inbox step or its status line:\n%s", stderr)
	}
	// A step the status line already names is not printed twice.
	timeout := &fakeTransport{result: core.Result{Status: core.ExitRefused, Code: core.CodeNotFound,
		Data: json.RawMessage(`{"message":"x","hint":"crosshall read","pending":[7],"owed_count":0}`)}}
	if _, stderr, _ := run(t, Env{Transport: timeout}, "reply", "2", "x"); strings.Contains(stderr, "next:") ||
		!strings.HasSuffix(stderr, "[crosshall] 1 unread, 0 owed. Run crosshall read.\n") {
		t.Fatalf("the pickup is named twice or not at all:\n%s", stderr)
	}
}

// One suggested wait length everywhere a wait is suggested, and for a member
// the hall delivers to no wait, but the line that says none is needed.
func TestEmptyInboxSuggestsTheStandardWait(t *testing.T) {
	ft := &fakeTransport{result: okResult(t, domain.InboxResult{})}
	if out, _, _ := run(t, Env{Transport: ft}, "inbox"); !strings.Contains(out, "crosshall wait --existing --work-only --timeout 9m") || strings.Contains(out, domain.HallDelivers) {
		t.Fatalf("empty inbox:\n%s", out)
	}
	ft = &fakeTransport{result: okResult(t, domain.InboxResult{Delivers: true})}
	if out, _, _ := run(t, Env{Transport: ft}, "inbox"); out != "Inbox is empty. The hall brings new work to this session: no wait is needed.\n" {
		t.Fatalf("empty inbox of a member the hall delivers to:\n%s", out)
	}
}
