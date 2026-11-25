package dispatch

import (
	"context"
	"database/sql"
	"path/filepath"
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
