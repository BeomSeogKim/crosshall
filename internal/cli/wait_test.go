package cli

import (
	"context"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
)

type waitTimingTransport struct {
	fakeTransport
	start  time.Time
	stopAt time.Duration
	polls  []time.Duration
}

func (f *waitTimingTransport) DispatcherRunning() bool {
	return f.stopAt == 0 || time.Since(f.start) < f.stopAt
}

func (f *waitTimingTransport) Do(ctx context.Context, call Call) (core.Result, error) {
	f.polls = append(f.polls, time.Since(f.start))
	return f.fakeTransport.Do(ctx, call)
}

func TestWaitPollsBackOffToOncePerSecond(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		transport := &waitTimingTransport{
			fakeTransport: fakeTransport{result: okResult(t, domain.WaitSnapshot{Cursor: domain.WaitCursor{BindingID: 1}})},
			start:         time.Now(),
		}
		_, stderr, code := run(t, Env{Transport: transport}, "wait", "--timeout", "4s")
		if elapsed := time.Since(transport.start); code != core.ExitWaitTimeout || elapsed != 4*time.Second {
			t.Fatalf("exit %d at %s, stderr %q", code, elapsed, stderr)
		}
		want := []time.Duration{0, 100 * time.Millisecond, 300 * time.Millisecond, 700 * time.Millisecond,
			1500 * time.Millisecond, 2500 * time.Millisecond, 3500 * time.Millisecond}
		if !slices.Equal(transport.polls, want) {
			t.Fatalf("polls at %v, want %v", transport.polls, want)
		}
	})
}

func TestTimedWaitKeepsDeadlineWhileRetryingUnavailablePolls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		transport := &waitTimingTransport{
			fakeTransport: fakeTransport{result: domain.Fail(core.ExitUnavailable, core.CodeUnsent, "unsent", "")},
			start:         time.Now(),
		}
		_, stderr, code := run(t, Env{Transport: transport}, "wait", "--timeout", "350ms")
		if elapsed := time.Since(transport.start); code != core.ExitWaitTimeout || elapsed != 350*time.Millisecond {
			t.Fatalf("exit %d at %s, stderr %q", code, elapsed, stderr)
		}
		want := []time.Duration{0, 100 * time.Millisecond, 300 * time.Millisecond}
		if !slices.Equal(transport.polls, want) {
			t.Fatalf("polls at %v, want %v", transport.polls, want)
		}
	})
}

func TestTimedWaitChecksDispatcherWhileBackedOff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		transport := &waitTimingTransport{
			fakeTransport: fakeTransport{result: okResult(t, domain.WaitSnapshot{Cursor: domain.WaitCursor{BindingID: 1}})},
			start:         time.Now(),
			stopAt:        3750 * time.Millisecond,
		}
		stdout, _, code := run(t, Env{Transport: transport}, "--json", "wait", "--timeout", "4s")
		if code != core.ExitUnavailable || !strings.Contains(stdout, `"code":"dispatcher_unavailable"`) {
			t.Fatalf("exit %d, stdout %q", code, stdout)
		}
		if last := transport.polls[len(transport.polls)-1]; last >= transport.stopAt {
			t.Fatalf("polled at %v after the dispatcher stopped at %v", last, transport.stopAt)
		}
	})
}

type waitTestTransport func(Call) (core.Result, error)

func (f waitTestTransport) Do(_ context.Context, call Call) (core.Result, error) {
	return f(call)
}

func TestWaitDeliversPendingWorkAfterDispatcherDowngrade(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cursor := domain.WaitCursor{BindingID: 1, MessageID: 3}
		transport := waitTestTransport(func(call Call) (core.Result, error) {
			cmd := call.Command.(domain.Wait)
			if cmd.Cursor == nil {
				return domain.OK(domain.WaitSnapshot{Cursor: cursor, CursorFingerprint: "new-dispatcher-fingerprint"})
			}
			if *cmd.Cursor != cursor {
				t.Fatalf("legacy poll got cursor %+v, want %+v", *cmd.Cursor, cursor)
			}
			return domain.OK(domain.WaitSnapshot{
				Cursor: domain.WaitCursor{BindingID: 1, MessageID: 4},
				InboxResult: domain.InboxResult{
					Unread: []domain.InboxItem{{ID: 4, Kind: "ask", Sender: "other"}},
				},
			})
		})
		stdout, stderr, code := run(t, Env{Transport: transport}, "--json", "wait", "--existing", "--work-only")
		if code != core.ExitOK || !strings.Contains(stdout, `"id":4`) || !strings.Contains(stdout, `"reason":"arrived"`) {
			t.Fatalf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
		}
	})
}

// A wait that times out has just checked the hall, so it names the next
// step itself: the pickup when a note waits that a work-only wait ignored,
// the wait again for a member that has to poll, and for one the hall
// delivers to the line that no wait is needed. It never sends the member to
// the inbox.
func TestWaitTimeoutNamesTheNextStep(t *testing.T) {
	for name, c := range map[string]struct {
		snapshot domain.WaitSnapshot
		want     string
	}{
		"clear, has to poll": {domain.WaitSnapshot{MemberInboxSummary: &domain.MemberInboxSummary{Pending: []int64{}}},
			"next: crosshall wait --existing --work-only --timeout 9m\n[crosshall] 0 unread, 0 owed.\n"},
		"clear, the hall delivers": {domain.WaitSnapshot{InboxResult: domain.InboxResult{Delivers: true},
			MemberInboxSummary: &domain.MemberInboxSummary{Pending: []int64{}}},
			"crosshall: no new messages before the wait timeout\nThe hall brings new work to this session: no wait is needed.\n[crosshall] 0 unread, 0 owed.\n"},
		"an unread note, the hall delivers": {domain.WaitSnapshot{InboxResult: domain.InboxResult{Delivers: true},
			MemberInboxSummary: &domain.MemberInboxSummary{Pending: []int64{8}}},
			"crosshall: no new messages before the wait timeout\n[crosshall] 1 unread, 0 owed. Run crosshall read.\n"},
		"an unread note": {domain.WaitSnapshot{MemberInboxSummary: &domain.MemberInboxSummary{Pending: []int64{8}, OwedCount: 1}},
			"crosshall: no new messages before the wait timeout\n[crosshall] 1 unread, 1 owed. Run crosshall read.\n"},
	} {
		synctest.Test(t, func(t *testing.T) {
			c.snapshot.Cursor = domain.WaitCursor{BindingID: 1, MessageID: 3}
			transport := waitTestTransport(func(Call) (core.Result, error) { return domain.OK(c.snapshot) })
			_, stderr, code := run(t, Env{Transport: transport}, "wait", "--existing", "--work-only", "--timeout", "3s")
			if code != core.ExitWaitTimeout || !strings.HasSuffix(stderr, c.want) || strings.Contains(stderr, "crosshall inbox") {
				t.Errorf("%s: exit %d, stderr %q", name, code, stderr)
			}
		})
	}
}
