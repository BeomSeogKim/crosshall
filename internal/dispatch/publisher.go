package dispatch

import (
	"cmp"
	"context"
	"errors"
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

const publishCoalesce = time.Second

// A member's resume notice is reserved at most resumeTries times per start,
// resumeRetry apart: a transport that refuses, or a fence that releases it,
// costs at most that many failures toward a suspension.
const (
	resumeTries = 2
	resumeRetry = 5 * time.Minute
)
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
	// withdrawRunning holds the attempts a withdrawal runs for: one at a
	// time per attempt.
	withdrawRunning sync.Map
	// withdrawing holds, per attempt and reason (withdrawKey), the time a
	// withdrawal may be tried again; a notice found not pending that still
	// carries due work is left to its receipt while it is listed for that
	// reason.
	withdrawing sync.Map
	// withdrawLogged holds the open notices whose unproven withdrawal was
	// logged.
	withdrawLogged sync.Map
	// confirming guards one approval check per approval fact: zero while
	// one runs, then the time it may be tried again.
	confirming sync.Map
	// proving guards one proof per route: zero while one runs, then the
	// time a failed proof may be retried (publishProveRetry).
	proving sync.Map
	workers sync.WaitGroup
	// homes names the configuration home setup installed an adapter's
	// hooks into, which a proof checks the listener against; nil names none.
	homes func(adapter string) string
	// stamps publishes the notice stamps a tick invalidated.
	stamps func(context.Context, store.Tx) error
	// resume holds, for this start only, the members still to get their
	// resume notice (binding -> the end of the window) and the resume
	// attempts in flight (attempt -> binding).
	resumeMu       sync.Mutex
	resume         map[int64]time.Time
	resumeAttempts map[int64]int64
	// resumeNext spaces the tries of a member: publishCoalesce while it
	// cannot take its resume yet (a running turn, a held lane), resumeRetry
	// after a reservation. resumeReserved counts its reservations.
	resumeNext     map[int64]time.Time
	resumeReserved map[int64]int
}

// resumeDue lists the members a tick tries to give their resume notice:
// in the window and without one in flight. It drops those past the window.
func (p *publisher) resumeDue(now time.Time) []int64 {
	p.resumeMu.Lock()
	defer p.resumeMu.Unlock()
	busy := make(map[int64]bool, len(p.resumeAttempts))
	for _, binding := range p.resumeAttempts {
		busy[binding] = true
	}
	var due []int64
	for binding, until := range p.resume {
		switch {
		case now.After(until):
			delete(p.resume, binding)
		case !busy[binding] && !now.Before(p.resumeNext[binding]):
			due = append(due, binding)
		}
	}
	return due
}

// resumeAttempt reports whether id is a resume attempt of this start.
func (p *publisher) resumeAttempt(id int64) (int64, bool) {
	p.resumeMu.Lock()
	defer p.resumeMu.Unlock()
	binding, ok := p.resumeAttempts[id]
	return binding, ok
}

// resumeDone settles a resume attempt: once the transport took it (or may
// have), the member has had its notice; released or not submitted, it
// stays due until its last try.
func (p *publisher) resumeDone(id, binding int64, submitted bool) {
	p.resumeMu.Lock()
	defer p.resumeMu.Unlock()
	delete(p.resumeAttempts, id)
	if submitted || p.resumeReserved[binding] >= resumeTries {
		delete(p.resume, binding)
	}
}

func (p *publisher) recover(ctx context.Context, now time.Time) error {
	return p.db.Update(ctx, func(tx store.Tx) error {
		if err := domain.SeedBuiltinEvidence(ctx, tx, p.evidence); err != nil {
			return err
		}
		if err := domain.RecoverPublish(ctx, tx, now); err != nil {
			return err
		}
		return p.applyStamps(ctx, tx)
	})
}

// applyStamps publishes what a transaction that may end an attempt
// invalidated: a notice that was not submitted gives its asks back to the
// turn-end line (#261).
func (p *publisher) applyStamps(ctx context.Context, tx store.Tx) error {
	if p.stamps == nil {
		return nil
	}
	return p.stamps(ctx, tx)
}

func (p *publisher) tick(ctx context.Context, now time.Time, startup bool) error {
	next := make(map[domain.PublishBinding]time.Time)
	var attempts []int64
	var queues []domain.HeldPublishQueue
	var withdrawals []domain.WithdrawCandidate
	// The notices whose withdrawal was logged, and which of them are open.
	var logged []int64
	p.withdrawLogged.Range(func(key, _ any) bool {
		logged = append(logged, key.(int64))
		return true
	})
	var open map[int64]bool
	var approvals []domain.ApprovalCheck
	var proofs []domain.PublishProof
	// What the tick's resume pass found, applied only once it commits: a
	// rolled-back attempt ID may be given to another attempt.
	var owing []domain.OwingMember
	var resumed map[int64]int64
	var settled, deferred []int64
	err := p.db.Update(ctx, func(tx store.Tx) error {
		owing, resumed, settled, deferred = nil, map[int64]int64{}, nil, nil
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
		var due []int64
		if startup {
			if owing, err = domain.OwingMembers(ctx, tx, now); err != nil {
				return err
			}
			for _, m := range owing {
				if m.CanResume() {
					due = append(due, m.BindingID)
				}
			}
		} else {
			due = p.resumeDue(now)
		}
		for _, binding := range due {
			id, owes, err := domain.ReserveResume(ctx, tx, binding, now)
			if err != nil {
				return err
			}
			switch {
			case !owes:
				settled = append(settled, binding)
			case id != 0:
				resumed[id] = binding
			default:
				deferred = append(deferred, binding)
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
		if err := domain.AlertLongHolds(ctx, tx, now); err != nil {
			return err
		}
		if err := p.applyStamps(ctx, tx); err != nil {
			return err
		}
		attempts, err = domain.ReservedPublishAttempts(ctx, tx)
		if err == nil {
			queues, err = domain.HeldPublishQueues(ctx, tx)
		}
		if err == nil {
			withdrawals, err = domain.WithdrawCandidates(ctx, tx)
		}
		if err == nil && len(logged) > 0 {
			open, err = domain.OpenPublishAttempts(ctx, tx, logged)
		}
		if err == nil {
			approvals, err = domain.ApprovalChecks(ctx, tx, now)
		}
		return err
	})
	if err != nil {
		return err
	}
	p.due = next
	p.resumeMu.Lock()
	if startup {
		p.resume, p.resumeAttempts = map[int64]time.Time{}, map[int64]int64{}
		p.resumeNext, p.resumeReserved = map[int64]time.Time{}, map[int64]int{}
		for _, m := range owing {
			if m.CanResume() {
				p.resume[m.BindingID] = now.Add(domain.ResumeWindow)
			}
		}
	}
	for _, binding := range settled {
		delete(p.resume, binding)
	}
	for id, binding := range resumed {
		p.resumeAttempts[id] = binding
		p.resumeReserved[binding]++
		p.resumeNext[binding] = now.Add(resumeRetry)
	}
	for _, binding := range deferred {
		p.resumeNext[binding] = now.Add(publishCoalesce)
	}
	p.resumeMu.Unlock()
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
	// Forget the retry times of attempts no longer listed for the same
	// reason (settled by a receipt or a withdrawal, or listed for another
	// reason), and the log marks of attempts that closed.
	candidate := make(map[withdrawKey]bool, len(withdrawals))
	for _, c := range withdrawals {
		candidate[withdrawKeyOf(c)] = true
	}
	p.withdrawing.Range(func(key, value any) bool {
		if !candidate[key.(withdrawKey)] {
			p.withdrawing.CompareAndDelete(key, value)
		}
		return true
	})
	for _, id := range logged {
		if !open[id] {
			p.withdrawLogged.Delete(id)
		}
	}
	for _, c := range withdrawals {
		withdraw := p.transport(c.Target.Adapter).Withdraw
		if withdraw == nil || p.homes == nil {
			continue
		}
		// Only through the home setup installed the hooks into, and only
		// the one the notice's lane names.
		c.Target.Home = p.homes(c.Target.Adapter)
		if home, err := filepath.EvalSymlinks(c.Target.Home); err != nil || home != c.LaneHome {
			continue
		}
		if retry, ok := p.withdrawing.Load(withdrawKeyOf(c)); ok && now.Before(retry.(time.Time)) {
			continue // waits to retry, or found it not pending for this reason
		}
		if _, running := p.withdrawRunning.LoadOrStore(c.ID, struct{}{}); running {
			continue // one withdrawal per attempt at a time, whatever its reason
		}
		p.workers.Go(func() {
			defer p.withdrawRunning.Delete(c.ID)
			p.withdraw(ctx, c, withdraw, now)
		})
	}
	due := make(map[int64]bool, len(approvals))
	for _, c := range approvals {
		due[c.FactSeq] = true
	}
	p.confirming.Range(func(key, value any) bool {
		if !due[key.(int64)] && !value.(time.Time).IsZero() {
			p.confirming.CompareAndDelete(key, value)
		}
		return true
	})
	for _, c := range approvals {
		confirm := p.transport(c.Target.Adapter).ConfirmApproval
		if confirm == nil || p.homes == nil {
			continue
		}
		c.Target.Home = p.homes(c.Target.Adapter)
		if home, err := filepath.EvalSymlinks(c.Target.Home); err != nil || home != c.RouteHome {
			continue
		}
		if after, seen := p.confirming.LoadOrStore(c.FactSeq, time.Time{}); seen {
			if retry := after.(time.Time); retry.IsZero() || now.Before(retry) {
				continue
			}
		}
		p.confirming.Store(c.FactSeq, time.Time{})
		p.workers.Go(func() { p.confirm(ctx, c, confirm, now) })
	}
	return nil
}

// confirm asks the member's server whether its approval request waits on
// its owner, and stores the answer only when it does. Absent, that fact
// is not asked about again; unknown, it is asked again after
// publishProveRetry.
func (p *publisher) confirm(ctx context.Context, c domain.ApprovalCheck,
	confirm func(context.Context, domain.PublishTarget) publish.Approval, at time.Time) {
	release := func() { p.confirming.Store(c.FactSeq, at.Add(publishProveRetry)) }
	call, cancel := context.WithTimeout(ctx, cmp.Or(p.timeout, publishTimeout))
	defer cancel()
	if call.Err() != nil {
		release()
		return
	}
	got, ok := bounded(call, cmp.Or(p.abandon, publishTimeout), func() publish.Approval {
		return confirm(call, c.Target)
	}, nil)
	switch {
	case !ok || call.Err() != nil || got == publish.ApprovalUnknown:
		release()
	case got == publish.ApprovalAbsent:
		p.confirming.Store(c.FactSeq, maxTime)
	case got == publish.ApprovalShown:
		record, cancelRecord := context.WithTimeout(context.WithoutCancel(ctx), publishTimeout)
		defer cancelRecord()
		err := p.db.Update(record, func(tx store.Tx) error {
			_, err := domain.ConfirmApproval(record, tx, c.Principal, c.FactSeq, time.Now())
			return err
		})
		logPublishFailure(err)
		if err != nil {
			release()
		} else {
			p.confirming.Store(c.FactSeq, maxTime)
		}
	default:
		release()
	}
}

// withdraw takes back a notice that should not run, and settles it when
// the transport proves it deleted, or reports it not pending while nothing
// it carries is due (#446). Not pending with due work, it is left to its
// receipt, and the held-notice backstop, for the rest of this start;
// unreachable, it is tried again after publishProveRetry.
func (p *publisher) withdraw(ctx context.Context, c domain.WithdrawCandidate,
	withdraw func(context.Context, domain.PublishTarget) publish.Withdrawal, at time.Time) {
	retry, key := at.Add(publishProveRetry), withdrawKeyOf(c)
	release := func() { p.withdrawing.Store(key, retry) }
	call, cancel := context.WithTimeout(ctx, cmp.Or(p.timeout, publishTimeout))
	defer cancel()
	if call.Err() != nil {
		release()
		return
	}
	got, ok := bounded(call, cmp.Or(p.abandon, publishTimeout), func() publish.Withdrawal {
		return withdraw(call, c.Target)
	}, nil)
	switch {
	case !ok || call.Err() != nil || got == publish.Unreachable:
		p.logWithdrawal(c.ID, publish.Unreachable)
		release()
	case got == publish.NotPending && c.Reason == domain.WithdrawNothingToRead:
		p.logWithdrawal(c.ID, got)
		var settled bool
		record, cancelRecord := context.WithTimeout(context.WithoutCancel(ctx), publishTimeout)
		defer cancelRecord()
		err := p.db.Update(record, func(tx store.Tx) error {
			var err error
			if settled, err = domain.SettleNotPendingAttempt(record, tx, c.ID, time.Now()); err != nil || !settled {
				return err
			}
			return p.applyStamps(record, tx)
		})
		logPublishFailure(err)
		switch {
		case err != nil:
			release()
		case settled:
			p.withdrawing.Delete(key)
		default:
			p.withdrawing.Store(key, maxTime)
		}
	case got == publish.NotPending:
		p.logWithdrawal(c.ID, got)
		p.withdrawing.Store(key, maxTime)
	case got == publish.Deleted:
		record, cancelRecord := context.WithTimeout(context.WithoutCancel(ctx), publishTimeout)
		defer cancelRecord()
		err := p.db.Update(record, func(tx store.Tx) error {
			if err := domain.WithdrawPublishAttempt(record, tx, c.ID, c.Reason, time.Now()); err != nil {
				return err
			}
			return p.applyStamps(record, tx)
		})
		logPublishFailure(err)
		if err != nil {
			release()
		} else {
			p.withdrawing.Delete(key)
		}
	default:
		release()
	}
}

// withdrawKey scopes a withdrawal's result: an attempt and the reason it is
// withdrawn for, so a notice listed again for another reason (its thread
// reopened) is withdrawn again (#447).
type withdrawKey struct {
	id     int64
	reason string
}

func withdrawKeyOf(c domain.WithdrawCandidate) withdrawKey { return withdrawKey{c.ID, c.Reason} }

// logWithdrawal logs, once per notice while it is open, a withdrawal that
// did not prove the notice gone.
func (p *publisher) logWithdrawal(id int64, got publish.Withdrawal) {
	if _, logged := p.withdrawLogged.LoadOrStore(id, struct{}{}); !logged {
		log.Printf("dispatcher: publish: withdrawal of notice %d ended %s", id, got)
	}
}

// maxTime is a retry time no tick reaches.
var maxTime = time.Unix(1<<62, 0)

func (p *publisher) submit(ctx context.Context, id int64) {
	var target domain.PublishTarget
	var ids []int64
	binding, resume := p.resumeAttempt(id)
	begin := domain.BeginPublish
	if resume {
		begin = domain.BeginResume
	}
	err := p.db.Update(ctx, func(tx store.Tx) error {
		var err error
		if target, ids, err = begin(ctx, tx, id, time.Now()); err != nil {
			return err
		}
		return p.applyStamps(ctx, tx)
	})
	if resume && (err != nil || len(ids) == 0) {
		p.resumeDone(id, binding, false)
	}
	if err != nil || len(ids) == 0 {
		logPublishFailure(err)
		return
	}
	if p.homes != nil {
		target.Home = p.homes(target.Adapter)
	}
	outcome := domain.PublishNotSubmitted // an adapter without a sender sends nothing
	if a, ok := publish.Lookup(target.Adapter); ok {
		line, ok := a.Notice(ids)
		if resume {
			line, ok = a.ResumeNotice(ids)
		}
		if ok && a.Transport().Send != nil {
			outcome = p.send(ctx, a.Transport().Send, target, line)
		}
	}
	if resume {
		p.resumeDone(id, binding, outcome != domain.PublishNotSubmitted)
	}
	p.record(ctx, id, outcome, time.Now())
}

func (p *publisher) record(ctx context.Context, id int64, outcome domain.PublishOutcome, at time.Time) {
	logged := false
	for {
		shutdown := ctx.Err() != nil
		record, cancel := context.WithTimeout(context.WithoutCancel(ctx), publishTimeout)
		err := p.db.Update(record, func(tx store.Tx) error {
			if err := domain.ResolvePublish(record, tx, id, outcome, at); err != nil {
				return err
			}
			return p.applyStamps(record, tx)
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
			if err := domain.RetirePublishQueue(probe, tx, queue, time.Now()); err != nil {
				return err
			}
			return p.applyStamps(probe, tx)
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
