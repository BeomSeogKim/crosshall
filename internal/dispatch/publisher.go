package dispatch

import (
	"context"
	"errors"
	"log"
	"sync"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/notice"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

const publishCoalesce = time.Second
const publishTimeout = time.Second
const publishRecordRetry = 250 * time.Millisecond

// PublishTransport is injected by publisher tests. Both callbacks must stop
// vendor work and return when canceled. Retired requires proof the queue is gone.
type PublishTransport struct {
	Send    func(context.Context, domain.PublishTarget, string) domain.PublishOutcome
	Retired func(context.Context, domain.PublishQueue) bool
}

type publisher struct {
	db        *store.Store
	transport PublishTransport
	due       map[domain.PublishBinding]time.Time
	working   sync.Map
	retiring  sync.Map
	workers   sync.WaitGroup
}

func (p *publisher) recover(ctx context.Context, now time.Time) error {
	return p.db.Update(ctx, func(tx store.Tx) error { return domain.RecoverPublish(ctx, tx, now) })
}

func (p *publisher) tick(ctx context.Context, now time.Time, startup bool) error {
	next := make(map[domain.PublishBinding]time.Time)
	var attempts []int64
	var queues []domain.PublishQueue
	err := p.db.Update(ctx, func(tx store.Tx) error {
		bindings, err := domain.PublishCandidates(ctx, tx)
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
					pending, err := domain.HasPublishWork(ctx, tx, binding)
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
		if err == nil && p.transport.Retired != nil {
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
	for _, queue := range queues {
		if _, running := p.retiring.LoadOrStore(queue, struct{}{}); running {
			continue
		}
		p.workers.Go(func() {
			defer p.retiring.Delete(queue)
			p.retire(ctx, queue)
		})
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
	outcome := p.send(ctx, target, notice.Claude(ids))
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

func (p *publisher) send(ctx context.Context, target domain.PublishTarget, line string) domain.PublishOutcome {
	ctx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()
	if ctx.Err() != nil {
		return domain.PublishNotSubmitted
	}
	outcome := p.transport.Send(ctx, target, line)
	if ctx.Err() != nil {
		return domain.PublishUncertain
	}
	return outcome
}

func (p *publisher) retire(ctx context.Context, queue domain.PublishQueue) {
	probe, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()
	if probe.Err() == nil && p.transport.Retired(probe, queue) && probe.Err() == nil {
		logPublishFailure(p.db.Update(probe, func(tx store.Tx) error {
			return domain.RetirePublishQueue(probe, tx, queue, time.Now())
		}))
	}
}

func logPublishFailure(err error) {
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Printf("dispatcher: publish: %v", err)
	}
}
