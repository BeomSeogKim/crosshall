package domain

import (
	"fmt"
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/publish"
)

// #288: a session whose agent does not start a turn when a background
// command exits (Codex) is told to end its turn instead of waiting, but
// only while its next prompt shows new work by the hall's own test (an
// enabled adapter, a hook revision with a prompt line, no do not disturb).
// Anywhere else it keeps the wait, and a member the hall delivers to needs
// neither.
func TestStandbyEndsTheTurnOnlyWhereTheNextPromptShowsWork(t *testing.T) {
	f, writer := adapterChannel(t)
	f.policy("codex", true, 1)
	promptLine := true
	f.svc.HasPromptLine = func(adapter, revision string) bool { return promptLine }
	s := f.newSession("codex")
	t0 := f.now.UnixMilli()
	f.hook(s, codexStart("startup", publish.ModeDaemon, t0))
	bid := bindingIDOf(t, f, f.principalOf(s))
	// standby reads the inbox, a wait's first snapshot and the snapshot of a
	// later poll, which carries the value the first one set.
	standby := func(who session) (inbox, first, later bool) {
		t.Helper()
		snap := decode[WaitSnapshot](t, f.run(who, Wait{Existing: true}, ""))
		next := decode[WaitSnapshot](t, f.run(who, Wait{Existing: true, Cursor: &snap.Cursor}, ""))
		return decode[InboxResult](t, f.run(who, Inbox{}, "")).AtNextPrompt, snap.AtNextPrompt, next.AtNextPrompt
	}
	want := func(name string, who session, at bool) {
		t.Helper()
		if inbox, first, later := standby(who); inbox != at || first != at || later != at {
			t.Errorf("%s: at_next_prompt inbox %v, first poll %v, later poll %v, want %v", name, inbox, first, later, at)
		}
	}

	want("hook-joined codex with a prompt line", s, true)
	want("joined by hand", writer, false)
	promptLine = false
	want("hook revision without a prompt line", s, false)
	promptLine = true
	f.policy("codex", false, 2)
	want("codex adapter off", s, false)
	f.policy("codex", true, 3)
	exec(t, f, fmt.Sprintf(`UPDATE binding SET dnd = 1 WHERE id = %d`, bid))
	want("do not disturb", s, false)
	exec(t, f, fmt.Sprintf(`UPDATE binding SET dnd = 0 WHERE id = %d`, bid))
	// A Codex launch is never verified, so its end does not end the launch:
	// a resumed thread's next prompt still shows new work.
	f.hook(s, codexEnded(publish.ModeDaemon, t0+1_000))
	want("after a codex session end", s, true)
}

// A Claude Code session wakes on a background wait, so it keeps it; once
// the hall delivers to it, it needs neither.
func TestStandbyKeepsTheWaitWhereABackgroundWaitWakes(t *testing.T) {
	p := newPublishFixture(t)
	p.svc.HasPromptLine = func(adapter, revision string) bool { return true }
	if inbox := decode[InboxResult](t, p.run(p.reader, Inbox{}, "")); inbox.AtNextPrompt || inbox.Delivers {
		t.Fatalf("claude candidate: %+v, want the wait", inbox)
	}
	p.proveRoute()
	if inbox := decode[InboxResult](t, p.run(p.reader, Inbox{}, "")); inbox.AtNextPrompt || !inbox.Delivers {
		t.Fatalf("claude delivered to: %+v, want no wait", inbox)
	}
}

func TestWaitTimeoutNamesTheStandbyForEachSession(t *testing.T) {
	for _, tc := range []struct {
		name               string
		summary            *MemberInboxSummary
		delivers, atPrompt bool
		hint               string
	}{
		{"has to poll", &MemberInboxSummary{Pending: []int64{}}, false, false, "crosshall wait --existing --work-only --timeout 9m"},
		{"delivered to", &MemberInboxSummary{Pending: []int64{}}, true, false, ""},
		{"at next prompt", &MemberInboxSummary{Pending: []int64{}}, false, true, AtNextPrompt},
		{"unread first", &MemberInboxSummary{Pending: []int64{4}}, false, true, "crosshall read"},
	} {
		result, err := WaitTimeout(tc.summary, tc.delivers, tc.atPrompt)
		if err != nil {
			t.Fatal(err)
		}
		if got := decode[Refusal](t, result).Hint; got != tc.hint {
			t.Errorf("%s: hint %q, want %q", tc.name, got, tc.hint)
		}
	}
}
