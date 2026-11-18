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
		Pending []int64 `json:"pending"`
	}{[]int64{id}})}
	stdout, stderr, code := run(t, Env{Transport: ft}, "inbox")
	if code != core.ExitOK || stderr != "" || !strings.HasSuffix(stdout, "[crosshall] pending: 9007199254740993\n") {
		t.Fatalf("plain %d stdout=%q stderr=%q", code, stdout, stderr)
	}

	stdout, stderr, code = run(t, Env{Transport: ft}, "inbox", "--json")
	var out struct {
		Pending []int64 `json:"pending"`
	}
	if err := json.Unmarshal([]byte(stdout), &out); code != core.ExitOK || stderr != "" || err != nil ||
		!reflect.DeepEqual(out.Pending, []int64{id}) || strings.Contains(stdout, "[crosshall] pending") {
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
