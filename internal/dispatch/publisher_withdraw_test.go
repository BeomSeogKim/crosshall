package dispatch

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

// withdrawWorld submits notice 11 through a transport that also
// withdraws, then moves the attempt onto a thread lane in home whose route
// is gone: a candidate the next tick withdraws.
func withdrawWorld(t *testing.T, withdraw func(context.Context, domain.PublishTarget) publish.Withdrawal) (*publishTestFixture, *publisher, string) {
	t.Helper()
	f := newPublishTestFixture(t)
	f.ask(t, 11)
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	useTransport(t, publish.Transport{
		Send: func(context.Context, domain.PublishTarget, string) domain.PublishOutcome {
			return domain.PublishSubmitted
		},
		Withdraw: withdraw,
	})
	p := &publisher{db: f.db, homes: func(string) string { return home }}
	t.Cleanup(p.workers.Wait)
	tickPublisher(t, p, f.now, true)
	p.workers.Wait()
	f.requireAttempt(t, 1, domain.PublishSubmitted)
	f.exec(t, "UPDATE publish_attempt SET lane = 'thread:claude:thread-1:"+home+"' WHERE id = 1")
	return f, p, home
}

func attemptRetired(t *testing.T, f *publishTestFixture) bool {
	t.Helper()
	var retired sql.NullInt64
	f.update(t, func(tx store.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT retired_at_ms FROM publish_attempt WHERE id = 1`).Scan(&retired)
	})
	return retired.Valid
}

// A proven deletion settles the notice; the transport is given the
// notice's thread, the installed home and the attempt's identity.
func TestPublisherSettlesANoticeItWithdrew(t *testing.T) {
	var got domain.PublishTarget
	var calls atomic.Int32
	f, p, home := withdrawWorld(t, func(_ context.Context, target domain.PublishTarget) publish.Withdrawal {
		calls.Add(1)
		got = target
		return publish.Deleted
	})
	tickPublisher(t, p, f.now.Add(time.Second), false)
	p.workers.Wait()
	if !attemptRetired(t, f) || calls.Load() != 1 {
		t.Fatalf("retired %v after %d withdrawals", attemptRetired(t, f), calls.Load())
	}
	if got.Address != "thread-1" || got.Home != home || !attemptIdentity.MatchString(got.Attempt) {
		t.Fatalf("withdrew %+v", got)
	}
	tickPublisher(t, p, f.now.Add(2*time.Second), false)
	p.workers.Wait()
	if calls.Load() != 1 {
		t.Fatal("a settled notice was withdrawn again")
	}
}

// Not pending, the notice is left to its receipt for the rest of the
// start: it is not settled and not tried again. Unreachable, it is tried
// again only after the retry interval.
func TestPublisherLeavesANoticeItCouldNotWithdraw(t *testing.T) {
	for _, tc := range []struct {
		result publish.Withdrawal
		retry  bool
	}{{publish.NotPending, false}, {publish.Unreachable, true}} {
		t.Run(string(tc.result), func(t *testing.T) {
			var calls atomic.Int32
			f, p, _ := withdrawWorld(t, func(context.Context, domain.PublishTarget) publish.Withdrawal {
				calls.Add(1)
				return tc.result
			})
			for _, after := range []time.Duration{time.Second, 2 * time.Second, publishProveRetry + 2*time.Second} {
				tickPublisher(t, p, f.now.Add(after), false)
				p.workers.Wait()
			}
			want := int32(1)
			if tc.retry {
				want = 2
			}
			if attemptRetired(t, f) || calls.Load() != want {
				t.Fatalf("retired %v after %d withdrawals, want unsettled after %d", attemptRetired(t, f), calls.Load(), want)
			}
		})
	}
}

// A withdrawal goes only through the home setup installed the hooks into,
// and only when that is the home the notice's lane names.
func TestPublisherWithdrawsOnlyThroughTheLanesHome(t *testing.T) {
	var calls atomic.Int32
	f, p, _ := withdrawWorld(t, func(context.Context, domain.PublishTarget) publish.Withdrawal {
		calls.Add(1)
		return publish.Deleted
	})
	p.homes = func(string) string { return t.TempDir() }
	tickPublisher(t, p, f.now.Add(time.Second), false)
	p.homes = nil
	tickPublisher(t, p, f.now.Add(2*time.Second), false)
	p.workers.Wait()
	if calls.Load() != 0 || attemptRetired(t, f) {
		t.Fatalf("withdrew %d times through another home", calls.Load())
	}
}

// An approval the server confirms or denies is not asked about again; one
// it cannot answer is asked again after the retry interval.
func TestPublisherAsksAboutAnApprovalUntilItsServerAnswers(t *testing.T) {
	f := newPublishTestFixture(t)
	p := &publisher{db: f.db}
	t.Cleanup(p.workers.Wait)
	for _, tc := range []struct {
		got   publish.Approval
		retry bool
	}{{publish.ApprovalShown, false}, {publish.ApprovalAbsent, false}, {publish.ApprovalUnknown, true}} {
		c := domain.ApprovalCheck{Principal: 2, FactSeq: 9000 + int64(len(tc.got))}
		var asked domain.PublishTarget
		p.confirm(t.Context(), c, func(_ context.Context, target domain.PublishTarget) publish.Approval {
			asked = target
			return tc.got
		}, f.now)
		after, _ := p.confirming.Load(c.FactSeq)
		retry := after.(time.Time)
		if tc.retry != (retry.Equal(f.now.Add(publishProveRetry))) || !tc.retry && retry != maxTime {
			t.Errorf("%s: retry at %v", tc.got, retry)
		}
		if asked != c.Target {
			t.Errorf("%s: asked about %+v", tc.got, asked)
		}
	}
}

// A withdrawal that does not prove the notice gone never frees its lane:
// new work waits for the notice's receipt (or the owner's release), so a
// notice that may still run is never followed by a second notice turn.
func TestPublisherSendsNoSecondNoticeBehindAnUnprovenWithdrawal(t *testing.T) {
	for _, result := range []publish.Withdrawal{publish.NotPending, publish.Unreachable} {
		t.Run(string(result), func(t *testing.T) {
			var sends atomic.Int32
			f := newPublishTestFixture(t)
			f.ask(t, 11)
			home, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			useTransport(t, publish.Transport{
				Send: func(context.Context, domain.PublishTarget, string) domain.PublishOutcome {
					sends.Add(1)
					return domain.PublishSubmitted
				},
				Withdraw: func(context.Context, domain.PublishTarget) publish.Withdrawal { return result },
			})
			p := &publisher{db: f.db, homes: func(string) string { return home }}
			t.Cleanup(p.workers.Wait)
			tickPublisher(t, p, f.now, true)
			p.workers.Wait()
			f.exec(t, "UPDATE publish_attempt SET lane = 'thread:claude:thread-1:"+home+"' WHERE id = 1")
			f.ask(t, 12)
			for _, after := range []time.Duration{2 * time.Second, 4 * time.Second, publishProveRetry + 6*time.Second} {
				tickPublisher(t, p, f.now.Add(after), false)
				p.workers.Wait()
			}
			if sends.Load() != 1 {
				t.Fatalf("%d notices sent; a second followed an unproven withdrawal", sends.Load())
			}
		})
	}
}

// threadNoticeWorld submits notice 11 on a thread lane in home whose route
// is active on that thread, through a transport that withdraws with
// result, and queues ask 12 behind it. Read, notice 11 has nothing left to
// read.
func threadNoticeWorld(t *testing.T, result publish.Withdrawal, read bool) (*publishTestFixture, *publisher, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	return threadNoticeWorldWith(t, func() publish.Withdrawal { return result }, read)
}

// threadNoticeWorldWith is threadNoticeWorld with a transport whose
// withdrawal answers withdraw().
func threadNoticeWorldWith(t *testing.T, withdraw func() publish.Withdrawal, read bool) (*publishTestFixture, *publisher, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var sends, withdrawals atomic.Int32
	f := newPublishTestFixture(t)
	f.ask(t, 11)
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	useTransport(t, publish.Transport{
		Send: func(context.Context, domain.PublishTarget, string) domain.PublishOutcome {
			sends.Add(1)
			return domain.PublishSubmitted
		},
		Withdraw: func(context.Context, domain.PublishTarget) publish.Withdrawal {
			withdrawals.Add(1)
			return withdraw()
		},
	})
	p := &publisher{db: f.db, homes: func(string) string { return home }}
	t.Cleanup(p.workers.Wait)
	tickPublisher(t, p, f.now, true)
	p.workers.Wait()
	f.requireAttempt(t, 1, domain.PublishSubmitted)
	f.exec(t, `UPDATE publish_attempt SET lane = 'thread:claude:thread-1:`+home+`' WHERE id = 1;
		UPDATE publish_route SET thread_id = 'thread-1', vendor_home = ?1`, home)
	if read {
		f.exec(t, `UPDATE recipient SET picked_up_at_ms = 1 WHERE message_id = 11`)
	}
	f.ask(t, 12)
	return f, p, &sends, &withdrawals
}

// tickThrough runs the publisher at each offset from the fixture's now.
func tickThrough(t *testing.T, f *publishTestFixture, p *publisher, offsets ...time.Duration) {
	t.Helper()
	for _, after := range offsets {
		tickPublisher(t, p, f.now.Add(after), false)
		p.workers.Wait()
	}
}

func settledReason(t *testing.T, f *publishTestFixture) string {
	t.Helper()
	var reason sql.NullString
	f.update(t, func(tx store.Tx) error {
		err := tx.QueryRowContext(t.Context(), `SELECT json_extract(detail, '$.reason') FROM event
			WHERE kind = 'publish_attempt_settled' AND json_extract(detail, '$.attempt') = '1'`).Scan(&reason)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	})
	return reason.String
}

// #446: a thread-lane notice the transport reports not pending, with
// nothing left to read, can do nothing more: its lane frees, the event says
// why, and the member's next notice is sent.
func TestPublisherFreesANoticeNotPendingWithNothingLeftToRead(t *testing.T) {
	f, p, sends, withdrawals := threadNoticeWorld(t, publish.NotPending, true)
	tickThrough(t, f, p, time.Second, 2*time.Second, 4*time.Second)
	if !attemptRetired(t, f) || withdrawals.Load() != 1 {
		t.Fatalf("retired %v after %d withdrawals", attemptRetired(t, f), withdrawals.Load())
	}
	if got, want := settledReason(t, f), "not pending: "+domain.WithdrawNothingToRead; got != want {
		t.Fatalf("settled for %q, want %q", got, want)
	}
	if sends.Load() != 2 {
		t.Fatalf("%d notices sent, want the next one offered", sends.Load())
	}
}

// #446: a notice that still carries due work is not withdrawn: its lane
// stays held, and the owner hears of the hold.
func TestPublisherHoldsANoticeWithDueWorkAndAlertsTheOwner(t *testing.T) {
	f, p, sends, withdrawals := threadNoticeWorld(t, publish.NotPending, false)
	tickThrough(t, f, p, time.Second, 16*time.Minute, 17*time.Minute)
	if attemptRetired(t, f) || withdrawals.Load() != 0 || sends.Load() != 1 {
		t.Fatalf("retired %v after %d withdrawals and %d sends; want held", attemptRetired(t, f), withdrawals.Load(), sends.Load())
	}
	var alerts int
	f.update(t, func(tx store.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM owner_alert o JOIN event e ON e.seq = o.event_seq
			WHERE e.kind = 'publish_held' AND json_extract(e.detail, '$.notice') = 1`).Scan(&alerts)
	})
	if alerts != 1 {
		t.Fatalf("%d publish_held alerts for the held notice, want 1", alerts)
	}
}

// #446: a withdrawal that ended not pending or unreachable is logged once
// for its notice, however often it is tried.
func TestPublisherLogsAnUnsettledWithdrawalOnce(t *testing.T) {
	for _, result := range []publish.Withdrawal{publish.NotPending, publish.Unreachable} {
		t.Run(string(result), func(t *testing.T) {
			var calls atomic.Int32
			f, p, _ := withdrawWorld(t, func(context.Context, domain.PublishTarget) publish.Withdrawal {
				calls.Add(1)
				return result
			})
			var logged bytes.Buffer
			previous := log.Writer()
			log.SetOutput(&logged)
			defer log.SetOutput(previous)
			tickThrough(t, f, p, time.Second, publishProveRetry+2*time.Second, 2*publishProveRetry+3*time.Second)
			log.SetOutput(previous)
			if result == publish.Unreachable && calls.Load() < 2 {
				t.Fatalf("%d withdrawals, want retries", calls.Load())
			}
			var lines []string
			for line := range strings.Lines(logged.String()) {
				if strings.Contains(line, "withdrawal of") {
					lines = append(lines, line)
				}
			}
			if len(lines) != 1 || !strings.Contains(lines[0], "notice 1 ") || !strings.Contains(lines[0], string(result)) {
				t.Fatalf("logged %q, want one line for notice 1 ending %s", logged.String(), result)
			}
		})
	}
}

func TestPublisherWithdrawsAgainWhenTheReasonChanges(t *testing.T) {
	f, p, sends, withdrawals := threadNoticeWorld(t, publish.NotPending, true)
	f.exec(t, `UPDATE publish_route SET state = 'ended', reason = 'session ended'`)
	tickThrough(t, f, p, time.Second, 2*time.Second)
	if attemptRetired(t, f) || withdrawals.Load() != 1 {
		t.Fatalf("ended: retired %v after %d withdrawals, want held after one", attemptRetired(t, f), withdrawals.Load())
	}
	f.exec(t, `UPDATE publish_route SET state = 'active', reason = ''`)
	tickThrough(t, f, p, 3*time.Second, 4*time.Second, 6*time.Second)
	if !attemptRetired(t, f) || withdrawals.Load() != 2 || sends.Load() != 2 {
		t.Fatalf("reopened: retired %v after %d withdrawals and %d sends", attemptRetired(t, f), withdrawals.Load(), sends.Load())
	}
}

func TestPublisherForgetsTheLogGuardOfASettledNotice(t *testing.T) {
	f, p, _, _ := threadNoticeWorld(t, publish.NotPending, true)
	tickThrough(t, f, p, time.Second, 2*time.Second)
	if !attemptRetired(t, f) {
		t.Fatal("the notice was not settled")
	}
	p.withdrawLogged.Range(func(key, _ any) bool {
		t.Errorf("log guard kept for notice %v", key)
		return true
	})
}

func TestPublisherRunsOneWithdrawalPerNoticeAcrossAReasonChange(t *testing.T) {
	gate, entered := make(chan struct{}), make(chan struct{}, 4)
	f, p, sends, withdrawals := threadNoticeWorldWith(t, func() publish.Withdrawal {
		entered <- struct{}{}
		<-gate
		return publish.NotPending
	}, true)
	var logged bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(previous)
	f.exec(t, `UPDATE publish_route SET state = 'ended', reason = 'session ended'`)
	tickPublisher(t, p, f.now.Add(time.Second), false)
	<-entered
	f.exec(t, `UPDATE publish_route SET state = 'active', reason = ''`)
	tickPublisher(t, p, f.now.Add(2*time.Second), false)
	select {
	case <-entered:
		close(gate)
		t.Fatal("a second withdrawal of the notice started while the first was in flight")
	case <-time.After(300 * time.Millisecond):
	}
	close(gate)
	p.workers.Wait()
	tickThrough(t, f, p, 3*time.Second, 4*time.Second, 6*time.Second)
	log.SetOutput(previous)
	if !attemptRetired(t, f) || withdrawals.Load() != 2 || sends.Load() != 2 {
		t.Fatalf("retired %v after %d withdrawals and %d sends", attemptRetired(t, f), withdrawals.Load(), sends.Load())
	}
	if n := strings.Count(logged.String(), "withdrawal of notice 1 "); n != 1 {
		t.Fatalf("logged notice 1 %d times: %q", n, logged.String())
	}
}

func TestPublisherLogsAnOpenNoticeOnceWhileItLeavesTheCandidates(t *testing.T) {
	var calls atomic.Int32
	f, p, home := withdrawWorld(t, func(context.Context, domain.PublishTarget) publish.Withdrawal {
		calls.Add(1)
		return publish.NotPending
	})
	var logged bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(previous)
	tickThrough(t, f, p, time.Second)
	f.exec(t, "UPDATE publish_attempt SET lane = 'process:2345:1234' WHERE id = 1")
	tickThrough(t, f, p, 2*time.Second)
	f.exec(t, "UPDATE publish_attempt SET lane = 'thread:claude:thread-1:"+home+"' WHERE id = 1")
	tickThrough(t, f, p, 3*time.Second)
	log.SetOutput(previous)
	if calls.Load() != 2 || attemptRetired(t, f) {
		t.Fatalf("%d withdrawals, retired %v; want the notice withdrawn again and held", calls.Load(), attemptRetired(t, f))
	}
	if n := strings.Count(logged.String(), "withdrawal of notice 1 "); n != 1 {
		t.Fatalf("logged notice 1 %d times: %q", n, logged.String())
	}
}
