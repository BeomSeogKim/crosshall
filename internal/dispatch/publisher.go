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
const publishProveRetry = 10 * time.Second

type publisher struct {
	db       *store.Store
	evidence []domain.BuiltinEvidence
	// timeout bounds each transport call and abandon the wait for a
	// canceled callback to return; zero means publishTimeout.
	timeout, abandon time.Duration
	due              map[domain.PublishBinding]time.Time
	working          sync.Map
	retiring         sync.Map
	// proving guards one proof per route: zero while one runs, then the
	// time a failed proof may be retried (publishProveRetry).
	proving sync.Map
	workers sync.WaitGroup
	// homes names the configuration home setup installed an adapter's
	// hooks into, which a proof checks the listener against; nil names none.
	homes func(adapter string) string
	// stamps publishes the notice stamps a tick invalidated.
	stamps func(context.Context, store.Tx) error
}

func (p *publisher) recover(ctx context.Context, now time.Time) error {
	return p.db.Update(ctx, func(tx store.Tx) error {
		if err := domain.SeedBuiltinEvidence(ctx, tx, p.evidence); err != nil {
			return err
		}
		return domain.RecoverPublish(ctx, tx, now)
	})
}

func (p *publisher) tick(ctx context.Context, now time.Time, startup bool) error {
	next := make(map[domain.PublishBinding]time.Time)
	var attempts []int64
	var queues []domain.HeldPublishQueue
	var proofs []domain.PublishProof
	err := p.db.Update(ctx, func(tx store.Tx) error {
		if err := domain.ActivatePublishRoutes(ctx, tx, now); err != nil {
			return err
		}
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
		if proofs, err = domain.UnprovenPublishRoutes(ctx, tx, now); err != nil {
			return err
		}
		// The sweep is the net under the event sites, and flushes the stamps
		// that a worker's transaction (a third failed submission) marked.
		if _, err := domain.MarkReachChanges(ctx, tx, now); err != nil {
			return err
		}
		if p.stamps != nil {
			if err := p.stamps(ctx, tx); err != nil {
				return err
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
	// Forget the retry time of a route that no longer waits for a proof
	// (proven, ended or a new generation); a running proof keeps its guard.
	waiting := make(map[domain.PublishBinding]bool, len(proofs))
	for _, proof := range proofs {
		waiting[proof.Binding] = true
	}
	p.proving.Range(func(key, value any) bool {
		if !waiting[key.(domain.PublishBinding)] && !value.(time.Time).IsZero() {
			p.proving.CompareAndDelete(key, value)
		}
		return true
	})
	for _, proof := range proofs {
		prove := p.transport(proof.Probe.Adapter).Prove
		if prove == nil {
			continue
		}
		if after, seen := p.proving.LoadOrStore(proof.Binding, time.Time{}); seen {
			if retry := after.(time.Time); retry.IsZero() || now.Before(retry) {
				continue // a proof runs, or failed and waits
			}
		}
		p.proving.Store(proof.Binding, time.Time{})
		p.workers.Go(func() { p.prove(ctx, proof, prove, now) })
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
	if a, ok := publish.Lookup(target.Adapter); ok {
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
	if a, ok := publish.Lookup(adapter); ok {
		return a.Transport()
	}
	return publish.Transport{}
}

// prove asks the adapter which process owns the route's address and
// records that proof, so the route can reserve a notice. Until a proof
// lands nothing is reserved for it.
func (p *publisher) prove(ctx context.Context, proof domain.PublishProof,
	prove func(context.Context, publish.Probe) (publish.Proven, bool), at time.Time) {
	// Like a probe, the guard outlives an abandoned proof: it is released
	// only when the callback returns.
	release := func() { p.proving.Store(proof.Binding, at.Add(publishProveRetry)) }
	if p.homes != nil {
		proof.Probe.InstalledHome = p.homes(proof.Probe.Adapter)
	}
	call, cancel := context.WithTimeout(ctx, cmp.Or(p.timeout, publishTimeout))
	defer cancel()
	if call.Err() != nil {
		release()
		return
	}
	type result struct {
		proven publish.Proven
		ok     bool
	}
	got, ok := bounded(call, cmp.Or(p.abandon, publishTimeout), func() result {
		proven, ok := prove(call, proof.Probe)
		return result{proven, ok}
	}, release)
	if !ok || !got.ok || call.Err() != nil {
		return
	}
	recorded := false
	err := p.db.Update(call, func(tx store.Tx) (err error) {
		recorded, err = domain.ConfirmPublishProof(call, tx, proof, got.proven)
		return err
	})
	if err == nil && recorded {
		p.proving.Delete(proof.Binding)
	}
	logPublishFailure(err)
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
