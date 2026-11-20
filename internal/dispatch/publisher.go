package dispatch

import (
	"cmp"
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

const publishCoalesce = time.Second
const publishTimeout = time.Second
const publishRecordRetry = 250 * time.Millisecond

type publisher struct {
	db *store.Store
	// adapters render each adapter's notices and carry its transport.
	adapters publish.Registry
	// profiles are the tested publish profiles this binary ships.
	profiles []domain.PublishProfile
	// timeout bounds each transport call and abandon the wait for a
	// canceled callback to return; zero means publishTimeout.
	timeout, abandon time.Duration
	due              map[domain.PublishBinding]time.Time
	working          sync.Map
	retiring         sync.Map
	workers          sync.WaitGroup
}

func (p *publisher) recover(ctx context.Context, now time.Time) error {
	return p.db.Update(ctx, func(tx store.Tx) error {
		if err := domain.SeedPublishProfiles(ctx, tx, p.profiles); err != nil {
			return err
		}
		return domain.RecoverPublish(ctx, tx, now)
	})
}

func (p *publisher) tick(ctx context.Context, now time.Time, startup bool) error {
	next := make(map[domain.PublishBinding]time.Time)
	var attempts []int64
	var queues []domain.HeldPublishQueue
	err := p.db.Update(ctx, func(tx store.Tx) error {
		bindings, err := domain.PublishCandidates(ctx, tx, now)
		if err != nil {
			return err
		}
		for _, binding := range bindings {
			due, held := p.due[binding]
			if !held {
				due = now
				if !startup {
					due = due.Add(publishCoalesce)
				}
			}
			next[binding] = due
			if !now.Before(due) {
				reserved, err := domain.ReservePublish(ctx, tx, binding, now)
				if err != nil {
					return err
				}
				if reserved {
					pending, err := domain.HasPublishWork(ctx, tx, binding, now)
					if err != nil {
						return err
					}
					if !pending {
						delete(next, binding)
					}
				}
			}
		}
		attempts, err = domain.ReservedPublishAttempts(ctx, tx)
		if err == nil {
			queues, err = domain.HeldPublishQueues(ctx, tx)
		}
		return err
	})
	if err != nil {
		return err
	}
	p.due = next
	for _, id := range attempts {
		if _, running := p.working.LoadOrStore(id, struct{}{}); running {
			continue
		}
		p.workers.Go(func() {
			defer p.working.Delete(id)
			p.submit(ctx, id)
		})
	}
	for _, held := range queues {
		retired := p.transport(held.Adapter).Retired
		if retired == nil {
			continue
		}
		if _, running := p.retiring.LoadOrStore(held.Queue, struct{}{}); running {
			continue
		}
		// The guard outlives an abandoned probe: it is released only when
		// the callback itself returns, so a stuck probe is never doubled.
		p.workers.Go(func() { p.retire(ctx, held.Queue, retired, func() { p.retiring.Delete(held.Queue) }) })
	}
	return nil
}

func (p *publisher) submit(ctx context.Context, id int64) {
	var target domain.PublishTarget
	var ids []int64
	err := p.db.Update(ctx, func(tx store.Tx) error {
		var err error
		target, ids, err = domain.BeginPublish(ctx, tx, id, time.Now())
		return err
	})
	if err != nil || len(ids) == 0 {
		logPublishFailure(err)
		return
	}
	outcome := domain.PublishNotSubmitted // an adapter without a sender sends nothing
	if a, ok := p.adapters[target.Adapter]; ok {
		if line, ok := a.Notice(ids); ok && a.Transport().Send != nil {
			outcome = p.send(ctx, a.Transport().Send, target, line)
		}
	}
	p.record(ctx, id, outcome, time.Now())
}

func (p *publisher) record(ctx context.Context, id int64, outcome domain.PublishOutcome, at time.Time) {
	logged := false
	for {
		shutdown := ctx.Err() != nil
		record, cancel := context.WithTimeout(context.WithoutCancel(ctx), publishTimeout)
		err := p.db.Update(record, func(tx store.Tx) error {
			return domain.ResolvePublish(record, tx, id, outcome, at)
		})
		cancel()
		if err == nil {
			return
		}
		if !logged {
			logPublishFailure(err)
			logged = true
		}
		if shutdown {
			return
		}
		select {
		case <-ctx.Done():
		case <-time.After(publishRecordRetry):
		}
	}
}

func (p *publisher) send(ctx context.Context, send func(context.Context, domain.PublishTarget, string) domain.PublishOutcome,
	target domain.PublishTarget, line string) domain.PublishOutcome {
	ctx, cancel := context.WithTimeout(ctx, cmp.Or(p.timeout, publishTimeout))
	defer cancel()
	if ctx.Err() != nil {
		return domain.PublishNotSubmitted
	}
	outcome, ok := bounded(ctx, cmp.Or(p.abandon, publishTimeout), func() domain.PublishOutcome {
		return send(ctx, target, line)
	}, nil)
	if !ok || ctx.Err() != nil {
		return domain.PublishUncertain
	}
	return outcome
}

func (p *publisher) transport(adapter string) publish.Transport {
	if a, ok := p.adapters[adapter]; ok {
		return a.Transport()
	}
	return publish.Transport{}
}

func (p *publisher) retire(ctx context.Context, queue domain.PublishQueue, retired func(context.Context, domain.PublishQueue) bool, release func()) {
	probe, cancel := context.WithTimeout(ctx, cmp.Or(p.timeout, publishTimeout))
	defer cancel()
	if probe.Err() != nil {
		release()
		return
	}
	gone, ok := bounded(probe, cmp.Or(p.abandon, publishTimeout), func() bool { return retired(probe, queue) }, release)
	if ok && gone && probe.Err() == nil {
		logPublishFailure(p.db.Update(probe, func(tx store.Tx) error {
			return domain.RetirePublishQueue(probe, tx, queue, time.Now())
		}))
	}
}

// bounded runs call until ctx ends, then waits at most abandon for it to
// return. ok is false when ctx ended first; a call that outlives abandon is
// left running and logged. A non-nil returned runs when call returns, even
// after bounded has given up on it.
func bounded[T any](ctx context.Context, abandon time.Duration, call func() T, returned func()) (result T, ok bool) {
	done := make(chan T, 1)
	go func() {
		done <- call()
		if returned != nil {
			returned()
		}
	}()
	select {
	case result = <-done:
		return result, true
	case <-ctx.Done():
	}
	select {
	case <-done:
	case <-time.After(abandon):
		log.Printf("dispatcher: publish: transport call did not return %v after its deadline", abandon)
	}
	return result, false
}

func logPublishFailure(err error) {
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("dispatcher: publish: %v", err)
	}
}
