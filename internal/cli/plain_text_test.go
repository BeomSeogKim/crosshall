package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/domain"
)

// Deadlines print in one local form across ask, read and waiting.
func TestPlainTimesUseOneForm(t *testing.T) {
	defer func(f func() time.Time) { now = f }(now)
	base := time.Date(2026, 11, 18, 13, 0, 0, 0, time.Local)
	now = func() time.Time { return base }
	due := base.Add(30 * time.Minute).UnixMilli()
	late := int64(90_000)

	read := &fakeTransport{result: okResult(t, domain.ReadResult{Messages: []domain.ReadMessage{
		{ID: 1, Kind: "ask", Channel: "checkout", Sender: "form", Body: "q", DueAtMS: &due},
	}})}
	out, _, _ := run(t, Env{Transport: read}, "read", "1")
	if !strings.Contains(out, "#1 ask from @form in #checkout, due 13:30\n") {
		t.Errorf("read: %s", out)
	}
	waiting := &fakeTransport{result: okResult(t, domain.WaitingResult{Requests: []domain.LedgerRequest{
		{ID: 1, Sender: "form", ExchangeID: 2, DueAtMS: due, Recipients: []domain.LedgerRecipient{
			{Alias: "tax", State: "replied", LateByMS: &late},
		}},
	}})}
	out, _, _ = run(t, Env{Transport: waiting}, "waiting")
	if !strings.Contains(out, "#1 from @form, due 13:30 (x2)\n") || !strings.Contains(out, "@tax replied, late by 1m\n") {
		t.Errorf("waiting: %s", out)
	}
	if strings.Contains(out, "T13:") || strings.Contains(out, "Z") {
		t.Errorf("an ISO time is left: %s", out)
	}
}

func TestJoinHintNamesTheMessageCommands(t *testing.T) {
	hint := domain.Hint("tax", "checkout")
	for _, want := range []string{"crosshall inbox", "crosshall read <id>", "crosshall reply <id>", "crosshall log", "crosshall note", "crosshall ask"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint lacks %q:\n%s", want, hint)
		}
	}
}

// Lateness under a second is still lateness, in the ledger and on reply.
func TestSubSecondLatenessIsShown(t *testing.T) {
	late := int64(640)
	status := &fakeTransport{result: okResult(t, domain.StatusResult{Channels: []domain.ChannelStatus{{Name: "checkout",
		Requests: []domain.LedgerRequest{{ID: 1, Sender: "form", ExchangeID: 2, Recipients: []domain.LedgerRecipient{
			{Alias: "tax", State: "replied", LateByMS: &late},
		}}}}}})}
	out, _, _ := run(t, Env{Transport: status, OwnerGate: func() string { return "" }}, "owner", "status")
	if !strings.Contains(out, "@tax replied, late by 640ms\n") {
		t.Errorf("owner status: %s", out)
	}
	reply := &fakeTransport{result: okResult(t, domain.ReplyResult{ID: 3, Ask: 1, To: "form", Outcome: "answer", LateByMS: late})}
	if out, _, _ := run(t, Env{Transport: reply}, "reply", "1", "x"); out != "#3 answers #1 (to @form). Late by 640ms.\n" {
		t.Errorf("reply: %q", out)
	}
}

func TestJoinHintStatesTheHistoryContract(t *testing.T) {
	hint := domain.Hint("tax", "checkout")
	for _, want := range []string{"before you joined", "marks nothing as read", "owe nothing"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint lacks %q:\n%s", want, hint)
		}
	}
}

// The turn-end rule is part of the protocol, stated once in the join hint in
// words that name no vendor and no adapter, so a shell-only member can follow
// it with crosshall read alone.
func TestJoinHintStatesTheTurnEndRule(t *testing.T) {
	hint := domain.Hint("tax", "checkout")
	const rule = "- Before voluntarily ending any turn while joined, leave no request addressed to you unread. The last `[crosshall] N unread, M owed.` line you got in this turn counts: at `0 unread` you may end the turn; otherwise run `crosshall read` and act on what it shows. If no command of this turn printed that line, run `crosshall read` once, unless the hall's last word to this session was that it brings new work to it: then a turn that showed no `[crosshall] pending:` line needs no check. Later arrivals belong to the next turn."
	if strings.Count(hint, rule) != 1 {
		t.Fatalf("hint lacks the turn-end rule exactly once:\n%s", hint)
	}
	for _, vendor := range []string{"Claude", "Codex", "Antigravity", "hook", "adapter", "vendor"} {
		if strings.Contains(strings.ToLower(hint), strings.ToLower(vendor)) {
			t.Errorf("hint names %q:\n%s", vendor, hint)
		}
	}
}
