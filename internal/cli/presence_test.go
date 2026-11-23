package cli

import (
	"strings"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
)

func TestRosterShowsTheObservedFactWithItsAge(t *testing.T) {
	ft := &fakeTransport{result: okResult(t, domain.WhoResult{Channel: "checkout", Members: []domain.Member{
		{Alias: "form", Vendor: "claude", State: "unknown", LastCommandAgoS: 12, Route: "pull", Self: true,
			Observed: &domain.Observed{Fact: domain.FactPromptSubmitted, Source: "claude", AgeS: 240, Current: true, Verified: true}},
		{Alias: "tax", Vendor: "claude", State: "unknown", LastCommandAgoS: 2000, Route: "pull",
			Observed: &domain.Observed{Fact: domain.FactPromptSubmitted, Source: "claude", AgeS: 1860}},
		{Alias: "lint", Vendor: "route", State: "unknown", LastCommandAgoS: 30, Route: "pull"},
	}})}
	out, _, code := run(t, Env{Transport: ft}, "who")
	if code != core.ExitOK {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{
		"prompt submitted 4m ago",
		"unknown (last: prompt submitted 31m ago, unverified)",
		"last command 30s ago",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "prompt submitted 4m ago, unverified") {
		t.Errorf("a verified launch is labelled unverified:\n%s", out)
	}
}

func TestTrailerShowsPresenceChangesAndTheBootstrapHint(t *testing.T) {
	ft := &fakeTransport{result: okResult(t, map[string]any{
		"unread": []any{}, "owed": []any{}, "pending": []int64{},
		"join_hint": "[crosshall] Joined #checkout as @claude1.",
		"roster_changes": []domain.Member{
			{Alias: "tax", Vendor: "claude", Change: "presence",
				Observed: &domain.Observed{Fact: domain.FactTurnFinishing, AgeS: 60, Current: true, Verified: true}},
		},
	})}
	out, _, code := run(t, Env{Transport: ft}, "inbox")
	if code != core.ExitOK {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"[crosshall] Joined #checkout as @claude1.", "[crosshall] @tax turn finished 1m ago"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "presence (") {
		t.Errorf("a presence change printed as a raw field:\n%s", out)
	}
}

func TestRosterSaysWhenAMemberIsWokenByItsWait(t *testing.T) {
	ft := &fakeTransport{result: okResult(t, domain.WhoResult{Channel: "checkout", Members: []domain.Member{
		{Alias: "lint", Vendor: "route", State: "unknown", LastCommandAgoS: 30, Route: "pull", WaitRunning: true},
		{Alias: "tax", Vendor: "route", State: "unknown", LastCommandAgoS: 30, Route: "pull"},
	}})}
	out, _, code := run(t, Env{Transport: ft}, "who")
	if code != core.ExitOK {
		t.Fatalf("exit %d", code)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "@lint") != strings.HasSuffix(line, "pull; woken by its wait") {
			t.Errorf("only @lint's line ends with its running wait: %q", line)
		}
	}
}
