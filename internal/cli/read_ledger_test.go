package cli

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
)

func TestLedgerCommandsUseExpectedIngressAndKeys(t *testing.T) {
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
		{[]string{"owner", "budget", "grant", "checkout/x3", "2"}, Call{Owner: true, Command: domain.BudgetGrant{Channel: "checkout", ExchangeID: 3, Units: 2}, OpKey: "test-key"}},
		{[]string{"owner", "budget", "grant", "checkout/@tax", "--roots", "1"}, Call{Owner: true, Command: domain.BudgetGrant{Channel: "checkout", Alias: "tax", Roots: 1}, OpKey: "test-key"}},
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
	if code != core.ExitOK || stderr != "" || !strings.HasSuffix(stdout, "[crosshall] 1 unread, 2 owed. Run crosshall inbox.\n") {
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

// The inbox pointer prints only when something waits. With nothing unread or
// owed a result carries no pointer, and a refusal whose next step is the
// inbox keeps that step.
func TestInboxPointerOnlyWhenSomethingWaits(t *testing.T) {
	trailer := func(pending []int64, owed int) *fakeTransport {
		return &fakeTransport{result: okResult(t, struct {
			Pending   []int64 `json:"pending"`
			OwedCount int     `json:"owed_count"`
		}{pending, owed})}
	}
	if out, _, _ := run(t, Env{Transport: trailer([]int64{}, 0)}, "read"); strings.Contains(out, "Run crosshall inbox") || strings.Contains(out, "0 unread") {
		t.Fatalf("pointer with nothing waiting:\n%s", out)
	}
	if out, _, _ := run(t, Env{Transport: trailer([]int64{}, 1)}, "read"); !strings.HasSuffix(out, "[crosshall] 0 unread, 1 owed. Run crosshall inbox.\n") {
		t.Fatalf("no pointer with an owed request:\n%s", out)
	}
	refusal := &fakeTransport{result: core.Result{Status: core.ExitRefused, Code: core.CodeCancelled,
		Data: json.RawMessage(`{"message":"the owner cancelled #2","hint":"crosshall inbox","pending":[],"owed_count":0}`)}}
	if _, stderr, _ := run(t, Env{Transport: refusal}, "reply", "2", "x"); !strings.Contains(stderr, "next: crosshall inbox\n") {
		t.Fatalf("a refusal lost its inbox step with nothing waiting:\n%s", stderr)
	}
}

// One suggested wait length everywhere a wait is suggested.
func TestEmptyInboxSuggestsTheStandardWait(t *testing.T) {
	ft := &fakeTransport{result: okResult(t, domain.InboxResult{})}
	if out, _, _ := run(t, Env{Transport: ft}, "inbox"); !strings.Contains(out, "crosshall wait --existing --work-only --timeout 9m") {
		t.Fatalf("empty inbox:\n%s", out)
	}
}
