package domain

import (
	"testing"

	"github.com/BeomSeogKim/crosshall/internal/publish"
)

// #288: a session whose agent does not start a turn when a background
// command exits (a hook-joined Codex session) is told to end its turn, not
// to wait. A member without such a session keeps the wait, and one the hall
// delivers to needs neither.
func TestStandbyEndsTheTurnWhereABackgroundWaitCannotWake(t *testing.T) {
	f, writer := adapterChannel(t)
	f.policy("codex", true, 1)
	s := f.newSession("codex")
	t0 := f.now.UnixMilli()
	f.hook(s, codexStart("startup", publish.ModeDaemon, t0))
	standby := func(who session) (inbox InboxResult, wait WaitSnapshot) {
		t.Helper()
		return decode[InboxResult](t, f.run(who, Inbox{}, "")), decode[WaitSnapshot](t, f.run(who, Wait{Existing: true}, ""))
	}

	inbox, wait := standby(s)
	if !inbox.AtNextPrompt || inbox.Delivers || !wait.AtNextPrompt || wait.Delivers {
		t.Fatalf("hook-joined codex: inbox %+v wait %+v, want at next prompt", inbox, wait)
	}
	// A session that joined by hand has no launch whose prompt hook shows work.
	if inbox, wait := standby(writer); inbox.AtNextPrompt || wait.AtNextPrompt {
		t.Fatalf("joined by hand: inbox %+v wait %+v, want the wait", inbox, wait)
	}
	// A Codex launch is never verified, so its end does not end the launch:
	// a resumed thread's next prompt still shows new work.
	f.hook(s, codexEnded(publish.ModeDaemon, t0+1_000))
	if inbox, wait := standby(s); !inbox.AtNextPrompt || !wait.AtNextPrompt {
		t.Fatalf("ended codex session: inbox %+v wait %+v, want at next prompt", inbox, wait)
	}
}

// A Claude Code session wakes on a background wait, so it keeps it; once
// the hall delivers to it, it needs neither.
func TestStandbyKeepsTheWaitWhereABackgroundWaitWakes(t *testing.T) {
	p := newPublishFixture(t)
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
