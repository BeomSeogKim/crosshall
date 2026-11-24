package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/domain"
)

func TestWaitingAndStatusShowPickupAgeAndLastObservedFact(t *testing.T) {
	defer func(f func() time.Time) { now = f }(now)
	base := time.Date(2026, 11, 18, 13, 36, 0, 0, time.Local)
	now = func() time.Time { return base.Add(2 * time.Hour) }
	picked := base.UnixMilli()
	generated := base.Add(2*time.Hour + 5*time.Minute).UnixMilli()
	fresh := base.Add(10 * time.Minute).UnixMilli()
	request := domain.LedgerRequest{ID: 204, Sender: "fable", ExchangeID: 15, DueAtMS: base.Add(3 * time.Hour).UnixMilli(),
		Recipients: []domain.LedgerRecipient{
			{Alias: "sol", State: "open", PickedUpAtMS: &picked,
				Observed: &domain.ObservedFact{Fact: domain.FactTurnFinishing, Source: "codex", AtMS: picked, FreshUntilMS: &fresh}},
			{Alias: "astra", State: "open",
				Observed: &domain.ObservedFact{Fact: domain.FactPromptSubmitted, Source: "codex", AtMS: generated - 60_000}},
			{Alias: "agy", State: "open"},
			{Alias: "lyra", State: "open", PickedUpAtMS: &picked,
				Observed: &domain.ObservedFact{Fact: domain.FactApprovalRequested, Source: "claude", AtMS: generated - 60_000, ApprovalOpen: true}},
			{Alias: "orion", State: "open", PickedUpAtMS: &picked,
				Observed: &domain.ObservedFact{Fact: domain.FactApprovalRequested, Source: "claude", AtMS: picked, FreshUntilMS: &fresh, ApprovalOpen: true}},
		}}
	for name, tc := range map[string]struct {
		result any
		args   []string
	}{
		"waiting": {domain.WaitingResult{GeneratedAtMS: generated, Channel: "dev", Requests: []domain.LedgerRequest{request}}, []string{"waiting"}},
		"status": {domain.StatusResult{GeneratedAtMS: generated, Channels: []domain.ChannelStatus{
			{Name: "dev", Requests: []domain.LedgerRequest{request}}}}, []string{"status"}},
	} {
		out, stderr, _ := run(t, Env{Transport: &fakeTransport{result: okResult(t, tc.result)}}, tc.args...)
		for _, want := range []string{
			"  @sol picked up 13:36 (2h ago); last observed: unknown (last: turn finished 2h ago, unverified)\n",
			"  @astra stored; last observed: prompt submitted 1m ago (unverified)\n",
			"  @agy stored\n",
			"  @lyra picked up 13:36 (2h ago); waits on its owner's approval since 15:40\n",
			// Past its cap a dialog is hedged, not "unknown": a denied one sends no Stop.
			"  @orion picked up 13:36 (2h ago); asked its owner for approval at 13:36, no answer reported\n",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("%s: missing %q in\n%s%s", name, want, out, stderr)
			}
		}
		for _, word := range []string{"stuck", "idle", "stale"} {
			if strings.Contains(out, word) {
				t.Errorf("%s: judgement word %q in\n%s", name, word, out)
			}
		}
	}
}

// The owner restore usage names where the binding ID comes from.
func TestOwnerRestoreUsageNamesWhereTheBindingIDComesFrom(t *testing.T) {
	_, stderr, code := run(t, Env{OwnerGate: func() string { return "" }}, "owner", "restore", "x")
	if code == 0 || !strings.Contains(stderr, "owner who --all") || !strings.Contains(stderr, "whoami --full") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
}
