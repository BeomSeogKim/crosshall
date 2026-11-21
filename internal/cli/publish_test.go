package cli

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

func TestPublishSwitchCommandsDispatch(t *testing.T) {
	for _, test := range []struct {
		args  []string
		owner bool
		want  domain.Command
	}{
		{[]string{"publish", "off"}, false, domain.Publish{}},
		{[]string{"publish", "on"}, false, domain.Publish{On: true}},
		{[]string{"owner", "publish", "hall", "off"}, true, domain.OwnerPublish{Hall: true, Switch: "off"}},
		{[]string{"owner", "publish", "member", "7", "default"}, true, domain.OwnerPublish{BindingID: 7, Switch: "default"}},
		{[]string{"owner", "publish", "resume", "7"}, true, domain.OwnerPublish{BindingID: 7, Switch: "resume"}},
	} {
		ft := &fakeTransport{result: okResult(t, domain.PublishSwitchResult{Switch: "off", Changed: true, Hall: true})}
		_, stderr, code := run(t, Env{Transport: ft}, append([]string{"--key", "switch-key"}, test.args...)...)
		if code != core.ExitOK || len(ft.calls) != 1 {
			t.Fatalf("%v: exit %d, calls %d, stderr %q", test.args, code, len(ft.calls), stderr)
		}
		if call := ft.calls[0]; call.Command != test.want || call.Owner != test.owner || call.OpKey != "switch-key" {
			t.Fatalf("%v: call %+v", test.args, call)
		}
	}
	for _, args := range [][]string{
		{"publish"}, {"publish", "maybe"}, {"publish", "on", "now"},
		{"owner", "publish", "hall", "default"}, {"owner", "publish", "member", "x", "on"},
		{"owner", "publish", "member", "7"}, {"owner", "publish", "resume", "0"}, {"owner", "publish"},
		{"owner", "adapter", "set", "claude", "--publish", "maybe"},
		{"owner", "adapter", "set", "claude", "--turn-end", "maybe"},
		{"owner", "adapter", "set", "claude", "--turn-end", "off", "--turn-end", "on"},
	} {
		ft := &fakeTransport{}
		if _, _, code := run(t, Env{Transport: ft}, args...); code != core.ExitUsage || len(ft.calls) != 0 {
			t.Errorf("%v: exit %d with %d calls, want a usage error", args, code, len(ft.calls))
		}
	}
}

func TestOwnerAdapterSetPublishSwitch(t *testing.T) {
	ft := &fakeTransport{result: okResult(t, store.AdapterPolicy{Adapter: "claude", Enabled: true, Publish: "off", Epoch: 4})}
	stdout, _, code := run(t, Env{Transport: ft}, "--key", "k", "owner", "adapter", "set", "claude", "--publish", "default")
	if code != core.ExitOK || stdout != "claude adapter enabled=on reminder=off publish=off epoch=4\n" {
		t.Fatalf("exit %d, stdout %q", code, stdout)
	}
	if cmd := ft.calls[0].Command.(domain.AdapterSet); cmd.Publish == nil || *cmd.Publish != "default" {
		t.Fatalf("call %+v", cmd)
	}
}

func TestPublishSwitchResultShowsTheEffectiveState(t *testing.T) {
	ft := &fakeTransport{result: okResult(t, domain.PublishSwitchResult{
		BindingID: 7, Channel: "dev", Alias: "opus", Switch: "on", Changed: true,
		Publish: &domain.PublishStatus{State: "off", Reason: "turned off by the owner for the hall"},
	})}
	stdout, _, _ := run(t, Env{Transport: ft}, "--key", "k", "publish", "on")
	want := "Publish switch on for @opus in #dev (binding 7).\n  publish: off (turned off by the owner for the hall)\n"
	if stdout != want {
		t.Fatalf("stdout %q, want %q", stdout, want)
	}
}

func TestPublishStatesRenderInWho(t *testing.T) {
	at := time.Now().Add(-time.Minute).UnixMilli()
	for _, test := range []struct {
		state domain.PublishStatus
		want  string
	}{
		{domain.PublishStatus{State: "available"}, "publish: available"},
		{domain.PublishStatus{State: "off", Reason: "turned off by the member"}, "publish: off (turned off by the member)"},
		{domain.PublishStatus{State: "held", Notice: 4, AtMS: at}, "publish: held: notice 4 unobserved since " + clock(at)},
		{domain.PublishStatus{State: "suspended", Reason: "notice 4 absorbed", AtMS: at, UntilMS: at + 1800000, Lift: "lifts then"},
			"publish: suspended: notice 4 absorbed at " + clock(at) + " until " + clock(at+1800000) + "; lifts then"},
		{domain.PublishStatus{State: "suspended"}, "publish: suspended"},
		{domain.PublishStatus{State: "capped", UntilMS: at}, "publish: capped until " + clock(at)},
	} {
		ft := &fakeTransport{result: okResult(t, domain.WhoResult{Channel: "dev", Members: []domain.Member{{
			Alias: "opus", Vendor: "claude", State: "unknown", Route: "pull", Publish: &test.state,
		}}})}
		stdout, _, _ := run(t, Env{Transport: ft}, "who")
		if !strings.Contains(stdout, "pull; "+test.want+"\n") {
			t.Errorf("who output lacks %q:\n%s", test.want, stdout)
		}
	}
}

func TestSendResultWordsForPublish(t *testing.T) {
	ft := &fakeTransport{result: okResult(t, domain.AskResult{ID: 42, Channel: "dev", Recipients: []domain.Delivery{
		{Alias: "queued", State: "stored", Next: "notice queued"},
		{Alias: "pull", State: "stored", Next: "waits for its next crosshall command"},
	}})}
	stdout, _, _ := run(t, Env{Transport: ft}, "--key", "k", "ask", "@queued", "work")
	for _, want := range []string{
		"@queued  stored -> notice queued\n",
		"@pull    stored -> waits for its next crosshall command\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("send result lacks %q:\n%s", want, stdout)
		}
	}
}

func TestWaitingShowsNoticeSubmissionObservationAndPickup(t *testing.T) {
	now := time.Now()
	fence, written := now.Add(-4*time.Minute).UnixMilli(), now.Add(-3*time.Minute).UnixMilli()
	observed, picked := now.Add(-2*time.Minute).UnixMilli(), now.Add(-time.Minute).UnixMilli()
	ft := &fakeTransport{result: okResult(t, domain.WaitingResult{GeneratedAtMS: now.UnixMilli(), Channel: "dev",
		Requests: []domain.LedgerRequest{{ID: 42, Sender: "lead", DueAtMS: now.Add(time.Hour).UnixMilli(), Recipients: []domain.LedgerRecipient{
			{Alias: "opus", State: "replied", PickedUpAtMS: &picked, Notice: &domain.LedgerNotice{
				ID: 7, State: "submitted", SubmittingAtMS: &fence, ResolvedAtMS: &written, Observation: "own_turn", ObservedAtMS: &observed},
				ReplyNotice: &domain.LedgerNotice{ID: 9, State: "submitted", SubmittingAtMS: &fence, ResolvedAtMS: &written}},
			{Alias: "held", State: "open", Notice: &domain.LedgerNotice{ID: 8, State: "uncertain", SubmittingAtMS: &fence, ResolvedAtMS: &written}},
			{Alias: "failed", State: "open", Notice: &domain.LedgerNotice{ID: 10, State: "not_submitted", SubmittingAtMS: &fence, ResolvedAtMS: &written}},
			{Alias: "pull", State: "open"},
		}}}})}
	stdout, _, _ := run(t, Env{Transport: ft}, "waiting")
	for _, want := range []string{
		"; notice 7 submitted " + ledgerTime(written) + ", observed " + ledgerTime(observed) + " (own turn); reply notice 9 submitted " +
			ledgerTime(written) + ", not observed\n",
		"@held stored; notice 8 submission uncertain since " + ledgerTime(written) + ", not observed\n",
		"@failed stored; notice 10 not submitted " + ledgerTime(written) + "\n",
		"@pull stored\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("waiting lacks %q:\n%s", want, stdout)
		}
	}
}

func TestResumeWordsAreHonest(t *testing.T) {
	for _, test := range []struct {
		state           domain.PublishStatus
		buildStopLifted bool
		want            string
	}{
		{domain.PublishStatus{State: "available"}, false,
			"Resume recorded for @opus in #dev (binding 7).\n  publish: available\n"},
		{domain.PublishStatus{State: "candidate", Reason: "no tested evidence for claude-2 on darwin-arm64 interactive"}, false,
			"Resume recorded for @opus in #dev (binding 7).\n  publish: candidate (no tested evidence for claude-2 on darwin-arm64 interactive)\n"},
		{domain.PublishStatus{State: "available"}, true,
			"Resume recorded for @opus in #dev (binding 7).\n  lifted the publish stop for this build across all members\n  publish: available\n"},
	} {
		ft := &fakeTransport{result: okResult(t, domain.PublishSwitchResult{BindingID: 7, Channel: "dev", Alias: "opus",
			Switch: "resume", Changed: true, BuildStopLifted: test.buildStopLifted, Publish: &test.state})}
		stdout, _, _ := run(t, Env{Transport: ft}, "--key", "k", "owner", "publish", "resume", "7")
		if stdout != test.want {
			t.Errorf("stdout %q, want %q", stdout, test.want)
		}
	}
}

func TestDoctorRendersPublishMembersAndCounters(t *testing.T) {
	data, _ := json.Marshal(map[string]any{
		"checks": []any{map[string]any{"name": "lock", "ok": true, "detail": "ok"}},
		"adapters": []any{map[string]any{
			"adapter": "claude", "install_state": "installed", "intended": 6, "installed": 6,
			"publish": store.DoctorPublishState{State: "candidate", Reason: "no active route",
				Counters: store.DoctorPublishCounters{Date: "20261121", Submitted: 3, Uncertain: 1, NotSubmitted: 2, OwnTurn: 2, Absorbed: 1},
				Members: []store.DoctorPublishMember{{Channel: "dev", Alias: "opus", BindingID: 7,
					State: store.PublishState{State: "off", Reason: "turned off by the owner for the hall"}}}},
		}},
	})
	env := Env{
		OwnerGate: func() string { return "" },
		Doctor:    func(context.Context) core.Result { return core.Result{Status: core.ExitOK, Data: data} },
	}
	stdout, _, _ := run(t, env, "owner", "doctor")
	for _, want := range []string{
		"  publish notices on 20261121 UTC: 3 submitted, 1 uncertain, 2 not submitted; observed 2 own turn, 1 absorbed, 0 unknown\n",
		"  publish for @opus in #dev (binding 7): off (turned off by the owner for the hall)\n",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("doctor lacks %q:\n%s", want, stdout)
		}
	}
}

func TestOwnerAdapterSetTurnEndSwitch(t *testing.T) {
	ft := &fakeTransport{result: okResult(t, store.AdapterPolicy{Adapter: "claude", Enabled: true, TurnEnd: "off", Epoch: 4})}
	stdout, _, code := run(t, Env{Transport: ft}, "--key", "k", "owner", "adapter", "set", "claude", "--turn-end", "off")
	if code != core.ExitOK || stdout != "claude adapter enabled=on reminder=off turn_end=off epoch=4\n" {
		t.Fatalf("exit %d, stdout %q", code, stdout)
	}
	if cmd := ft.calls[0].Command.(domain.AdapterSet); cmd.TurnEnd == nil || *cmd.TurnEnd != "off" || cmd.Publish != nil {
		t.Fatalf("call %+v", cmd)
	}
}
