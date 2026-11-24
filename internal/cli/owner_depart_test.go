package cli

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/setuptest"
)

const departureToken = "0123456789abcdef01234567"

func TestOwnerDepartUsesBindingTokenAndOperationKey(t *testing.T) {
	ft := &fakeTransport{result: okResult(t, domain.OwnerDepartResult{BindingID: 12, Channel: "checkout", Alias: "writer"})}
	env := Env{Transport: ft, OwnerGate: func() string { return "" }}
	out, stderr, code := run(t, env, "--key", "departure-1", "owner", "depart", "12", "--seen", departureToken)
	if code != core.ExitOK || out != "@writer in #checkout departed (binding 12).\n" {
		t.Fatalf("exit %d: stdout %q, stderr %q", code, out, stderr)
	}
	want := Call{Owner: true, Command: domain.OwnerDepart{BindingID: 12, Seen: departureToken}, OpKey: "departure-1", OwnerAlerts: true}
	if len(ft.calls) != 1 || ft.calls[0] != want {
		t.Fatalf("calls = %+v, want %+v", ft.calls, want)
	}
}

func TestOwnerDepartChecksGateAndArgumentsBeforeSending(t *testing.T) {
	ft := &fakeTransport{}
	env := Env{Transport: ft, Start: func(context.Context) error {
		t.Fatal("invalid owner depart started the dispatcher")
		return nil
	}}
	for _, args := range [][]string{
		{"owner", "depart"},
		{"owner", "depart", "0", "--seen", departureToken},
		{"owner", "depart", "12", "--seen", strings.ToUpper(departureToken)},
		{"owner", "depart", "12", "--seen", departureToken, "extra"},
	} {
		_, stderr, code := run(t, env, args...)
		if code != core.ExitUsage || !strings.Contains(stderr, "crosshall owner who --all") {
			t.Errorf("%v: exit %d, stderr %q", args, code, stderr)
		}
	}
	env.OwnerGate = func() string { return "owner shell required" }
	_, _, code := run(t, env, "owner", "depart", "12", "--seen", departureToken)
	if code != core.ExitRefused || len(ft.calls) != 0 {
		t.Fatalf("exit %d, calls %+v", code, ft.calls)
	}
}

func TestOwnerWhoShowsEvidenceAndDepartureDetails(t *testing.T) {
	result := domain.OwnerWhoResult{Channels: []domain.OwnerWhoChannel{{
		Name: "checkout",
		Members: []domain.OwnerMember{
			{
				Member: domain.Member{BindingID: 12, Alias: "writer", LastCommandAgoS: 9,
					Observed: &domain.Observed{Fact: domain.FactPromptSubmitted, AgeS: 82, Current: true, Verified: true}},
				BindingState: "live", LastEvidence: &domain.OwnerEvidence{Fact: "command", Source: "cli", AgeS: 9},
				OpenRequests: []int64{17, 28}, DepartToken: departureToken,
			},
			{
				Member: domain.Member{BindingID: 13, Alias: "reader"}, BindingState: "live", EvidenceStale: true,
				DepartToken: strings.Repeat("a", 24),
			},
		},
	}}}
	env := Env{Transport: &fakeTransport{result: okResult(t, result)}}
	out, stderr, code := run(t, env, "owner", "who", "--all")
	if code != core.ExitOK {
		t.Fatalf("exit %d, stderr %q", code, stderr)
	}
	for _, want := range []string{"@writer [binding 12] live", "last command 9s ago", "last observed: prompt submitted 1m ago",
		"open requests: #17 #28", "depart token: " + departureToken, "@reader [binding 13] live, stale", "last evidence: unknown"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %q", want, out)
		}
	}
	if strings.Count(out, "last command") != 1 {
		t.Fatalf("unexpected last-command text in %q", out)
	}
	if strings.Count(out, "crosshall owner depart <binding-id> --seen <token>") != 1 {
		t.Fatalf("expected a single instruction for the listing: %q", out)
	}
}

func TestWaitingMarksDepartedRecipientWithoutReplacingPickup(t *testing.T) {
	picked := int64(60_000)
	result := domain.WaitingResult{GeneratedAtMS: 120_000, Requests: []domain.LedgerRequest{{
		ID: 17, Recipients: []domain.LedgerRecipient{{Alias: "writer", State: "open", PickedUpAtMS: &picked, BindingState: "departed"}},
	}}}
	env := Env{Transport: &fakeTransport{result: okResult(t, result)}}
	out, stderr, code := run(t, env, "waiting")
	if code != core.ExitOK || !strings.Contains(out, "@writer picked up ") || !strings.Contains(out, "(1m ago), recipient departed") {
		t.Fatalf("exit %d: stdout %q, stderr %q", code, out, stderr)
	}
}

func TestReadmeOwnerWhoMatchesTheCommand(t *testing.T) {
	result := domain.OwnerWhoResult{Channels: []domain.OwnerWhoChannel{{
		Name: "shop-api",
		Members: []domain.OwnerMember{
			{Member: domain.Member{Alias: "claude1", Vendor: "claude", BindingID: 1, LastCommandAgoS: 1,
				Publish: &domain.PublishStatus{State: "available"}},
				BindingState: "live", Reach: "notice", ExitTracking: domain.ExitByHooks, DepartToken: departureToken},
			{Member: domain.Member{Alias: "codex1", Vendor: "codex", BindingID: 2,
				Publish:  &domain.PublishStatus{State: "candidate", Reason: "no sender for codex"},
				Observed: &domain.Observed{Fact: domain.FactPromptSubmitted, Source: "codex", Current: true}},
				BindingState: "live", Reach: "prompt", ExitTracking: domain.ExitUntracked, DepartToken: strings.Repeat("a", 24)},
		},
	}}}
	env := Env{Transport: &fakeTransport{result: okResult(t, result)}}
	args := []string{"owner", "who", "--all"}
	out, stderr, code := run(t, env, args...)
	if code != core.ExitOK || stderr != "" {
		t.Fatalf("owner who exited %d: stdout %q, stderr %q", code, out, stderr)
	}
	token := regexp.MustCompile(`(; depart token: )[0-9a-f]{24}\n`)
	got := "$ crosshall " + strings.Join(args, " ") + "\n" + out
	want := setuptest.ReadmeBlock(t, "owner-who")
	if len(token.FindAllString(got, -1)) != 2 || len(token.FindAllString(want, -1)) != 2 {
		t.Fatalf("expected two 24-hex departure tokens in the owner view and the README\ngot:\n%s\nwant:\n%s", got, want)
	}
	got = token.ReplaceAllString(got, "${1}<token>\n")
	want = token.ReplaceAllString(want, "${1}<token>\n")
	if got != want {
		t.Fatalf("README owner view differs from the command\ngot:\n%s\nwant:\n%s", got, want)
	}
}

func TestOwnerWhoRendersReachAndStoppedBuild(t *testing.T) {
	for _, tc := range []struct {
		name, reach, route, state, want string
	}{
		{"notice", "notice", "pull", "live", "the hall starts its turn"},
		{"prompt", "prompt", "pull", "live", "sees work at its next prompt"},
		{"pull", "pull", "pull", "live", "pull only: sees work when it runs a crosshall command"},
		{"departed", "", "unavailable", "departed", "unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := domain.OwnerWhoResult{Channels: []domain.OwnerWhoChannel{{Name: "checkout", Members: []domain.OwnerMember{{
				Member: domain.Member{BindingID: 3, Alias: "reader", Vendor: "claude", Route: tc.route},
				Reach:  tc.reach, BindingState: tc.state,
			}}}}}
			env := Env{Transport: &fakeTransport{result: okResult(t, result)}}
			out, stderr, code := run(t, env, "owner", "who", "--all")
			want := "  @reader [binding 3] " + tc.state + ", claude, focus unset; " + tc.want + ", last evidence: unknown (joined 0s ago)\n"
			if code != core.ExitOK || !strings.Contains(out, want) {
				t.Fatalf("exit %d, stderr %q, stdout %q, want %q", code, stderr, out, want)
			}
		})
	}
	for _, tc := range []struct {
		name, origin, text string
	}{
		{"stopped origin", "reader", " (notice 41 to @reader, absorbed). After checking that session, turn them back on with: "},
		{"stopped peer", "", " (notice 41, absorbed). The session that received it is gone; turn notices back on with: "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := domain.OwnerWhoResult{Channels: []domain.OwnerWhoChannel{{Name: "checkout", Members: []domain.OwnerMember{{
				Member: domain.Member{BindingID: 3, Alias: "reader", Vendor: "claude", Route: "pull",
					Publish: &domain.PublishStatus{State: "suspended", Reason: "absorbed"}},
				Reach: "prompt", BindingState: "live", DepartToken: departureToken,
			}}}}, StoppedBuilds: []domain.OwnerStoppedBuild{{
				Adapter: "claude", Version: "2.1.331", Platform: "darwin-arm64", Mode: "interactive",
				StoppedAtMS: 60_000, Notice: 41, Reason: "absorbed", ResumeBindingID: 3, OriginAlias: tc.origin,
			}}}
			env := Env{Transport: &fakeTransport{result: okResult(t, result)}}
			out, stderr, code := run(t, env, "owner", "who", "--all")
			footer := "Notices to Claude Code 2.1.331 are stopped since " + clock(60_000) + tc.text + "crosshall owner publish resume 3"
			if code != core.ExitOK || !strings.Contains(out, "sees work at its next prompt; publish: suspended") ||
				!strings.Contains(out, footer+"\nTo depart a listed binding") {
				t.Fatalf("exit %d, stderr %q, stdout %q, want footer %q after the member", code, stderr, out, footer)
			}
		})
	}
}

func TestOwnerWhoNamesRequestsNothingWillTell(t *testing.T) {
	member := func(alias string, binding int64, stuck *domain.UnreadNotWoken) domain.OwnerMember {
		return domain.OwnerMember{Member: domain.Member{BindingID: binding, Alias: alias, Vendor: "claude", Route: "pull"},
			Reach: "pull", BindingState: "live", UnreadNotWoken: stuck}
	}
	result := domain.OwnerWhoResult{Channels: []domain.OwnerWhoChannel{
		{Name: "dev", Members: []domain.OwnerMember{
			member("sol", 1, &domain.UnreadNotWoken{Requests: 2, OldestAgeS: 41 * 60}),
			member("opus", 2, nil),
		}},
		{Name: "ops", Members: []domain.OwnerMember{member("agy", 3, &domain.UnreadNotWoken{Requests: 1, OldestAgeS: 5 * 60})}},
	}}
	out, stderr, code := run(t, Env{Transport: &fakeTransport{result: okResult(t, result)}}, "owner", "who", "--all")
	lines := strings.Split(out, "\n")
	if code != core.ExitOK || lines[0] != "3 requests wait on members the hall cannot wake: @sol 2 (oldest 41m), @agy 1." || lines[1] != "#dev" {
		t.Fatalf("exit %d, stderr %q, stdout:\n%s", code, stderr, out)
	}
	for _, want := range []string{"@sol [binding 1]", "; 2 unread requests, oldest 41m\n", "@agy [binding 3]", "; 1 unread request, 5m old\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("stdout lacks %q:\n%s", want, out)
		}
	}
	if opus := strings.SplitN(strings.Split(out, "@opus")[1], "\n", 2)[0]; strings.Contains(opus, "unread") {
		t.Fatalf("a member without stuck requests got a count:\n%s", out)
	}

	result.Channels = result.Channels[1:]
	out, _, _ = run(t, Env{Transport: &fakeTransport{result: okResult(t, result)}}, "owner", "who", "--all")
	if first := strings.Split(out, "\n")[0]; first != "1 request waits on a member the hall cannot wake: @agy 1." {
		t.Fatalf("one request: %q", first)
	}
	result.Channels[0].Members[0].UnreadNotWoken = nil
	out, _, _ = run(t, Env{Transport: &fakeTransport{result: okResult(t, result)}}, "owner", "who", "--all")
	if !strings.HasPrefix(out, "#ops\n") {
		t.Fatalf("a banner without stuck requests:\n%s", out)
	}
}
