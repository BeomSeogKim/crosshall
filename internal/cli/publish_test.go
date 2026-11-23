package cli

import (
	"context"
	"encoding/json"
	"regexp"
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

// Every fixed reason word has one sentence, the same in a send result, in
// who and in waiting, and only a recipient nothing will tell gets the next:
// line.
func TestWakeWordsRenderTheSameEverywhere(t *testing.T) {
	until := time.Now().Add(20 * time.Minute).UnixMilli()
	none := func(why, seen string) domain.Wake { return domain.Wake{Wake: domain.WakeNone, Why: why, SeenAt: seen} }
	codex := none(domain.WhyNoRoute, domain.SeenAtPrompt)
	codex.Adapter = "codex"
	suspended := none(domain.WhySuspended, domain.SeenAtPrompt)
	suspended.UntilMS = until
	capped := none(domain.WhyCapped, domain.SeenAtPrompt)
	capped.UntilMS = until
	cases := []struct {
		alias string
		wake  domain.Wake
		text  string
	}{
		{"opus", domain.Wake{Wake: domain.WakeNotice}, "notice queued"},
		{"vega", domain.Wake{Wake: domain.WakeWait}, "its wait is running"},
		{"sol", codex, "not woken: no notice route for codex; it sees this at its next prompt"},
		{"agy", none(domain.WhyNoRoute, domain.SeenAtCommand), "not woken: no notice route; it sees this at its next crosshall command"},
		{"astra", none(domain.WhyUnproven, domain.SeenAtPrompt), "not woken: notices not proven for its build; it sees this at its next prompt"},
		{"lead", suspended, "not woken: notices suspended until " + clock(until) + "; it sees this at its next prompt"},
		{"lyra", none(domain.WhyOff, domain.SeenAtCommand), "not woken: notices off; it sees this at its next crosshall command"},
		{"recv", capped, "not woken: notice limit until " + clock(until) + "; it sees this at its next prompt"},
		{"fable", none(domain.WhyHeld, domain.SeenAtPrompt), "not woken: an earlier notice is unobserved; it sees this at its next prompt"},
		{"orion", none(domain.WhyKind, domain.SeenAtPrompt), "not woken: this message starts no turn; it sees this at its next prompt"},
	}
	var recipients []domain.Delivery
	var ledger []domain.LedgerRecipient
	var members []domain.Member
	for _, c := range cases {
		recipients = append(recipients, domain.Delivery{Alias: c.alias, State: "stored", Next: "old words", Wake: c.wake})
		ledger = append(ledger, domain.LedgerRecipient{Alias: c.alias, State: "open", Wake: c.wake})
		members = append(members, domain.Member{Alias: c.alias, Vendor: "claude", State: "unknown", Route: "pull", Wake: c.wake})
	}
	ask, _, _ := run(t, Env{Transport: &fakeTransport{result: okResult(t, domain.AskResult{ID: 42, Channel: "dev", Recipients: recipients})}},
		"--key", "k", "ask", "@opus", "work")
	waiting, _, _ := run(t, Env{Transport: &fakeTransport{result: okResult(t, domain.WaitingResult{Channel: "dev",
		Requests: []domain.LedgerRequest{{ID: 42, Sender: "lead", Recipients: ledger}}})}}, "waiting")
	who, _, _ := run(t, Env{Transport: &fakeTransport{result: okResult(t, domain.WhoResult{Channel: "dev", Members: members})}}, "who")
	for _, c := range cases {
		if !regexp.MustCompile(`@` + c.alias + ` +stored -> ` + regexp.QuoteMeta(c.text) + "\n").MatchString(ask) {
			t.Errorf("send result lacks @%s %q:\n%s", c.alias, c.text, ask)
		}
		if !strings.Contains(waiting, "@"+c.alias+" stored -> "+c.text+"\n") {
			t.Errorf("waiting lacks @%s %q:\n%s", c.alias, c.text, waiting)
		}
		member, _, _ := strings.Cut(c.text, ";")
		switch c.wake.Wake {
		case domain.WakeNotice:
			member = "woken by a notice"
		case domain.WakeWait:
			member = "woken by its wait"
		}
		if !regexp.MustCompile(`@` + c.alias + ` .*  ` + regexp.QuoteMeta(member) + "\n").MatchString(who) {
			t.Errorf("who lacks @%s %q:\n%s", c.alias, member, who)
		}
	}
	const next = "next: nothing will tell @sol, @agy, @astra, @lead, @lyra, @recv, @fable or @orion. " +
		"Ask a member the hall can wake (crosshall who), or tell your user.\n"
	if strings.Count(ask, "next:") != 1 || !strings.Contains(ask, next) {
		t.Errorf("send result lacks the one next: line %q:\n%s", next, ask)
	}
	if strings.Contains(waiting, "next:") {
		t.Errorf("waiting printed a next: line:\n%s", waiting)
	}
}

func TestNoNextLineWhenEveryRecipientIsTold(t *testing.T) {
	notice := domain.Wake{Wake: domain.WakeNotice}
	ask, _, _ := run(t, Env{Transport: &fakeTransport{result: okResult(t, domain.AskResult{ID: 42, Channel: "dev",
		Recipients: []domain.Delivery{{Alias: "opus", State: "stored", Wake: notice}, {Alias: "sol", State: "picked_up"}}})}},
		"--key", "k", "ask", "@opus", "work")
	none := domain.Wake{Wake: domain.WakeNone, Why: domain.WhyKind, SeenAt: domain.SeenAtPrompt}
	note, _, _ := run(t, Env{Transport: &fakeTransport{result: okResult(t, domain.NoteResult{ID: 43, Channel: "dev",
		Recipients: []domain.Delivery{{Alias: "opus", State: "stored", Wake: none}}})}}, "--key", "k", "note", "@opus", "fyi")
	more, _, _ := run(t, Env{Transport: &fakeTransport{result: okResult(t, domain.ReplyResult{ID: 44, Ask: 42, To: "lead",
		Outcome: domain.OutcomeMoreTime, Delivery: domain.Delivery{Alias: "lead", State: "stored", Wake: none}})}},
		"--key", "k", "reply", "42", "--more-time", "soon")
	answer, _, _ := run(t, Env{Transport: &fakeTransport{result: okResult(t, domain.ReplyResult{ID: 45, Ask: 42, To: "lead",
		Delivery: domain.Delivery{Alias: "lead", State: "stored", Wake: none}})}}, "--key", "k", "reply", "42", "done")
	for name, out := range map[string]string{"ask to a notice": ask, "note": note, "more-time reply": more} {
		if strings.Contains(out, "next:") {
			t.Errorf("%s printed a next: line:\n%s", name, out)
		}
	}
	if !strings.Contains(note, "@opus  stored -> not woken: a note starts no turn; it sees this at its next prompt\n") {
		t.Errorf("note result:\n%s", note)
	}
	if !strings.Contains(answer, "next: nothing will tell @lead. Ask a member the hall can wake (crosshall who), or tell your user.\n") {
		t.Errorf("final reply lacks its next: line:\n%s", answer)
	}
}

// A recipient the turn-end check of its running turn will tell is not named
// in the next: line.
func TestTurnEndRecipientIsLeftOutOfTheNextLine(t *testing.T) {
	turnEnd := domain.Wake{Wake: domain.WakeNone, Why: domain.WhyOff, SeenAt: domain.SeenAtTurnEnd}
	command := domain.Wake{Wake: domain.WakeNone, Why: domain.WhyNoRoute, SeenAt: domain.SeenAtCommand}
	ask, _, _ := run(t, Env{Transport: &fakeTransport{result: okResult(t, domain.AskResult{ID: 42, Channel: "dev",
		Recipients: []domain.Delivery{{Alias: "opus", State: "stored", Wake: turnEnd}, {Alias: "agy", State: "stored", Wake: command}}})}},
		"--key", "k", "ask", "@opus", "work")
	for _, want := range []string{
		"@opus  stored -> not woken: notices off; it sees this when its turn ends\n",
		"next: nothing will tell @agy. Ask a member the hall can wake (crosshall who), or tell your user.\n",
	} {
		if !strings.Contains(ask, want) {
			t.Errorf("send result lacks %q:\n%s", want, ask)
		}
	}
	only, _, _ := run(t, Env{Transport: &fakeTransport{result: okResult(t, domain.AskResult{ID: 43, Channel: "dev",
		Recipients: []domain.Delivery{{Alias: "opus", State: "stored", Wake: turnEnd}}})}}, "--key", "k", "ask", "@opus", "work")
	if strings.Contains(only, "next:") {
		t.Errorf("a recipient its turn's end will tell got a next: line:\n%s", only)
	}
}

func TestWaitingSaysWhenAnUnreadRequestWasShown(t *testing.T) {
	shown := time.Now().Add(-5 * time.Minute).UnixMilli()
	none := domain.Wake{Wake: domain.WakeNone, Why: domain.WhyNoRoute, Adapter: "codex"}
	waiting, _, _ := run(t, Env{Transport: &fakeTransport{result: okResult(t, domain.WaitingResult{Channel: "dev",
		Requests: []domain.LedgerRequest{{ID: 42, Sender: "lead", Recipients: []domain.LedgerRecipient{
			{Alias: "sol", State: "open", Wake: none, ShownAtMS: &shown}}}}})}}, "waiting")
	want := "@sol stored -> not woken: no notice route for codex; shown " + clock(shown) + ", not read\n"
	if !strings.Contains(waiting, want) {
		t.Errorf("waiting lacks %q:\n%s", want, waiting)
	}
}
