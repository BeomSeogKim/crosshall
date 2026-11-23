package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
)

func TestOwnerLineFollowsTheOutputOnStderr(t *testing.T) {
	alerts := []string{"crosshall: round limit reached in #dev on x12 at 14:31.", "next: crosshall owner status --channel dev shows the thread."}
	refused := domain.Refuse(core.CodeNotFound, "no exchange x9 in #dev", "crosshall owner status --channel dev")
	for _, tc := range []struct {
		name   string
		result core.Result
		args   []string
	}{
		{"text", okResult(t, domain.StatusResult{Channels: []domain.ChannelStatus{{Name: "dev"}}}), []string{"owner", "status"}},
		{"json", okResult(t, domain.StatusResult{Channels: []domain.ChannelStatus{{Name: "dev"}}}), []string{"--json", "owner", "status"}},
		{"refused text", refused, []string{"owner", "budget", "grant", "dev/x9"}},
		{"refused json", refused, []string{"--json", "owner", "budget", "grant", "dev/x9"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plainOut, plainErr, plainCode := run(t, Env{Transport: &fakeTransport{result: tc.result}}, tc.args...)
			alerted := tc.result
			alerted.OwnerAlerts = alerts
			out, stderr, code := run(t, Env{Transport: &fakeTransport{result: alerted}}, tc.args...)
			if out != plainOut || code != plainCode || stderr != plainErr+strings.Join(alerts, "\n")+"\n" {
				t.Fatalf("exit %d stdout %q stderr %q; without the lines: exit %d stdout %q stderr %q",
					code, out, stderr, plainCode, plainOut, plainErr)
			}
		})
	}
}

func TestOwnerStatusShowsWhatAStopRefusedAndHowToLiftIt(t *testing.T) {
	defer func(f func() time.Time) { now = f }(now)
	base := time.Date(2026, 11, 18, 14, 31, 0, 0, time.Local)
	now = func() time.Time { return base }
	at := base.UnixMilli()
	var requests []domain.StopRequest
	for id := int64(41); id <= 50; id++ {
		requests = append(requests, domain.StopRequest{ID: id, Sender: "a", Recipients: []string{"b"}})
	}
	status := domain.StatusResult{Channels: []domain.ChannelStatus{{Name: "dev", GuardStops: []domain.GuardStop{
		{ExchangeID: 12, AtMS: at, Round: 9, RoundLimit: 8, Sender: "a", Recipients: []string{"b"}, Requests: requests[:8], RequestCount: 8},
		{ExchangeID: 13, AtMS: at, Round: 9, RoundLimit: 8, Sender: "b", Recipients: []string{"a", "c"}, Requests: requests, RequestCount: 60},
		{Alias: "tax", BindingID: 3, BindingState: "live", AtMS: at, Round: 1, RoundLimit: 0, Sender: "tax", Recipients: []string{"a"}},
		{Alias: "tax", BindingID: 4, BindingState: "departed", AtMS: at, Round: 9, RoundLimit: 8, Sender: "tax", Recipients: []string{"a"}},
	}}}}
	out, stderr, code := run(t, Env{Transport: &fakeTransport{result: okResult(t, status)}}, "owner", "status")
	want := "#dev\n" +
		"  x12 stopped at 14:31 (round limit 8)\n" +
		"    refused: @a to @b, round 9\n" +
		"    since its last reset: #41 @a to @b, #42 @a to @b, #43 @a to @b, #44 @a to @b, #45 @a to @b, #46 @a to @b, #47 @a to @b, #48 @a to @b\n" +
		"    to let it continue: crosshall owner budget grant dev/x12 --rounds 8\n" +
		"  x13 stopped at 14:31 (round limit 8)\n" +
		"    refused: @b to @a and @c, round 9\n" +
		"    since its last reset: #41 @a to @b, #42 @a to @b, ... 53 more ..., #46 @a to @b, #47 @a to @b, #48 @a to @b, #49 @a to @b, #50 @a to @b\n" +
		"    to let it continue: crosshall owner budget grant dev/x13 --rounds 8\n" +
		"  @tax stopped at 14:31 (new threads, round limit 0)\n" +
		"    refused: @tax to @a, round 1\n" +
		"    to let it continue: crosshall owner budget grant dev/@tax --rounds 8\n" +
		"  @tax (departed, binding 4) stopped at 14:31 (new threads, round limit 8)\n" +
		"    refused: @tax to @a, round 9\n" +
		"    to let it continue: crosshall owner budget grant 4 --rounds 8\n"
	if code != core.ExitOK || stderr != "" || out != want {
		t.Fatalf("exit %d stderr %q\nstdout:\n%s\nwant:\n%s", code, stderr, out, want)
	}
}

func TestBudgetGrantTakesABindingID(t *testing.T) {
	five := int64(5)
	for _, tc := range []struct {
		args []string
		want domain.BudgetGrant
	}{
		{[]string{"7"}, domain.BudgetGrant{BindingID: 7}},
		{[]string{"7", "--rounds", "5"}, domain.BudgetGrant{BindingID: 7, Rounds: &five}},
	} {
		ft := &fakeTransport{result: okResult(t, domain.BudgetGrantResult{Channel: "dev", Alias: "tax", BindingID: 7, Rounds: &five})}
		out, stderr, code := run(t, Env{Transport: ft}, append([]string{"owner", "budget", "grant"}, tc.args...)...)
		got, _ := ft.calls[0].Command.(domain.BudgetGrant)
		if code != core.ExitOK || stderr != "" || got.BindingID != tc.want.BindingID || (got.Rounds == nil) != (tc.want.Rounds == nil) ||
			got.Rounds != nil && *got.Rounds != 5 || got.Channel != "" || !ft.calls[0].Owner || ft.calls[0].OpKey == "" ||
			out != "binding 7 (@tax in #dev): reset. Its new threads may run 5 rounds ahead.\n" {
			t.Fatalf("%v: exit %d stdout %q stderr %q call %+v", tc.args, code, out, stderr, ft.calls)
		}
	}
	for _, args := range [][]string{{"0"}, {"-3"}, {"7", "3"}, {"7", "--rounds", "-1"}, {"7", "--units", "3"}} {
		ft := &fakeTransport{}
		_, stderr, code := run(t, Env{Transport: ft}, append([]string{"owner", "budget", "grant"}, args...)...)
		if code != core.ExitUsage || !strings.Contains(stderr, "<binding-id>") || len(ft.calls) != 0 {
			t.Fatalf("%v: exit %d stderr %q calls %+v", args, code, stderr, ft.calls)
		}
	}
	if out, _, _ := run(t, Env{}, "owner", "help"); !strings.Contains(out, "owner budget grant <binding-id> [--rounds <n>]") {
		t.Fatalf("owner help lacks the binding form:\n%s", out)
	}
}
