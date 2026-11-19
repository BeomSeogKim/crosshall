package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
)

const departureToken = "0123456789abcdef01234567"

func TestOwnerDepartUsesBindingTokenAndOperationKey(t *testing.T) {
	ft := &fakeTransport{result: okResult(t, domain.OwnerDepartResult{BindingID: 12, Channel: "checkout", Alias: "writer"})}
	env := Env{Transport: ft, OwnerGate: func() string { return "" }}
	out, stderr, code := run(t, env, "--key", "departure-1", "owner", "depart", "12", "--seen", departureToken)
	if code != core.ExitOK || out != "@writer in #checkout departed (binding 12).\n" {
		t.Fatalf("exit %d: stdout %q, stderr %q", code, out, stderr)
	}
	want := Call{Owner: true, Command: domain.OwnerDepart{BindingID: 12, Seen: departureToken}, OpKey: "departure-1"}
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
