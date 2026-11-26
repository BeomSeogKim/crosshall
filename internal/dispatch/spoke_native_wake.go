package dispatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strconv"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/ident"
	"github.com/BeomSeogKim/crosshall/internal/link"
	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/setup"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

const (
	spokeWakePoll  = time.Second
	spokeWakeBatch = 8
)

type spokeWakeWorker struct {
	e          *spokeEvents
	dispatcher context.Context
	stateRoot  string
	enrollment string
	poll       chan struct{}
	replay     chan link.Wake
	ready      chan []link.WakeRoute
	next       int
}

func (w *spokeWakeWorker) run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.poll:
			routes, err := w.readyRoutes(ctx)
			logPublishFailure(err)
			select {
			case w.ready <- routes:
			case <-ctx.Done():
				return
			}
		case wake := <-w.replay:
			logPublishFailure(w.execute(ctx, wake))
		}
	}
}

func (w *spokeWakeWorker) readyRoutes(ctx context.Context) ([]link.WakeRoute, error) {
	var local []spokeNativeRoute
	if err := w.e.db.View(ctx, func(tx store.Tx) error {
		var err error
		local, err = readSpokeNativeRoutes(ctx, tx, w.e.boot)
		return err
	}); err != nil {
		return nil, err
	}
	if len(local) == 0 {
		w.next = 0
		return nil, nil
	}
	if w.next >= len(local) {
		w.next = 0
	}
	start := w.next
	w.next = (w.next + min(spokeWakeBatch, len(local))) % len(local)
	var out []link.WakeRoute
	for i := range min(spokeWakeBatch, len(local)) {
		candidate := local[(start+i)%len(local)]
		ready, _, _, ok, err := w.prove(ctx, candidate)
		if err != nil {
			return out, err
		}
		if ok {
			out = append(out, ready)
		}
	}
	return out, nil
}

func (w *spokeWakeWorker) prove(ctx context.Context, candidate spokeNativeRoute) (link.WakeRoute, publish.Target, publish.Adapter, bool, error) {
	probe := candidate.Probe
	adapter, ok := publish.Lookup(candidate.Member.Vendor)
	if !ok {
		return link.WakeRoute{}, publish.Target{}, nil, false, nil
	}
	transport := adapter.Transport()
	if transport.Prove == nil || transport.Send == nil || adapter.Reach(probe.Route) != "" {
		return link.WakeRoute{}, publish.Target{}, nil, false, nil
	}
	address, reason := adapter.Resolve(probe.Route)
	if address == "" || reason != "" {
		return link.WakeRoute{}, publish.Target{}, nil, false, nil
	}
	probe.Adapter, probe.Address = candidate.Member.Vendor, address
	probe.InstalledHome = setup.InstalledConfigDir(w.stateRoot, probe.Adapter)
	probe.Home = probe.InstalledHome
	budget, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()
	type proof struct {
		value publish.Proven
		ok    bool
	}
	proven, completed := bounded(budget, publishTimeout, func() proof {
		value, ok := transport.Prove(budget, probe)
		return proof{value, ok}
	}, nil)
	if !completed || !proven.ok || budget.Err() != nil || !domain.PublishLaunchOwns(probe, proven.value) {
		return link.WakeRoute{}, publish.Target{}, nil, false, nil
	}
	probe.Target.Queue = proven.value.Queue
	lane, err := w.lane(ctx, probe.Target, adapter.ThreadRouted())
	if err != nil {
		return link.WakeRoute{}, publish.Target{}, nil, false, err
	}
	return link.WakeRoute{Member: candidate.Member, Scope: candidate.Scope, Boot: candidate.Boot,
		Launch: candidate.Launch, PolicyEpoch: candidate.PolicyEpoch, Lane: lane}, probe.Target, adapter, true, nil
}

func (w *spokeWakeWorker) lane(ctx context.Context, target publish.Target, thread bool) (string, error) {
	parts := []string{strconv.FormatInt(target.Queue.PID, 10), strconv.FormatInt(target.Queue.StartMS, 10)}
	if thread {
		parts = []string{target.Adapter, target.Address, target.Home}
	}
	key, err := json.Marshal(parts)
	if err != nil {
		return "", err
	}
	var lane string
	read := func(tx store.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT lane FROM spoke_native_lane WHERE target = ?`, string(key)).Scan(&lane)
	}
	if err := w.e.db.View(ctx, read); !errors.Is(err, sql.ErrNoRows) {
		return lane, err
	}
	err = w.e.db.Update(ctx, func(tx store.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO spoke_native_lane (target, lane) VALUES (?, ?) ON CONFLICT (target) DO NOTHING`,
			string(key), link.NewNonce()); err != nil {
			return err
		}
		return read(tx)
	})
	return lane, err
}

func (w *spokeWakeWorker) execute(ctx context.Context, wake link.Wake) error {
	var candidate spokeNativeRoute
	var found bool
	var staged bool
	if err := w.e.db.View(ctx, func(tx store.Tx) error {
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM spoke_wake_attempt
			WHERE attempt_id = ? AND state = 'staged')`, wake.AttemptID).Scan(&staged); err != nil || !staged {
			return err
		}
		var err error
		candidate, found, err = readSpokeNativeRoute(ctx, tx, w.e.boot, wake.Route.Member)
		return err
	}); err != nil || !staged {
		return err
	}
	if !found {
		_, err := w.e.finishWake(ctx, wake.AttemptID, link.WakeNotSubmitted)
		return err
	}
	route, target, adapter, ok, err := w.prove(ctx, candidate)
	if err != nil {
		return err
	}
	if !ok || route != wake.Route {
		_, err = w.e.finishWake(ctx, wake.AttemptID, link.WakeNotSubmitted)
		return err
	}
	line, ok := adapter.Notice(wake.IDs)
	if !ok {
		_, err = w.e.finishWake(ctx, wake.AttemptID, link.WakeNotSubmitted)
		return err
	}
	won, err := w.claim(ctx, wake, candidate)
	if err != nil || !won {
		return err
	}
	target.Attempt = "link-" + w.enrollment + "-" + strconv.FormatInt(wake.AttemptID, 10)
	outcome := publish.NotSubmitted
	if !w.e.stop.halted.Load() {
		outcome = (&publisher{}).send(ctx, adapter.Transport().Send, target, line)
	}
	for {
		shutdown := w.dispatcher.Err() != nil
		record, cancel := context.WithTimeout(context.WithoutCancel(w.dispatcher), publishTimeout)
		_, err := w.e.finishWake(record, wake.AttemptID, string(outcome))
		cancel()
		if err == nil || shutdown {
			return err
		}
		select {
		case <-w.dispatcher.Done():
		case <-time.After(publishRecordRetry):
		}
	}
}

func (w *spokeWakeWorker) claim(ctx context.Context, wake link.Wake, local spokeNativeRoute) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, publishTimeout)
	defer cancel()
	held, err := w.e.hold(ctx, wake.Route.Member, wake.Route.Scope)
	if err != nil {
		return false, err
	}
	defer held.release()
	if held.absent || w.e.stop.halted.Load() {
		return false, nil
	}
	stamp, err := ident.LookupMembership(held.dir)
	if err != nil || stamp == nil || link.Membership(*stamp) != wake.Route.Scope.Membership {
		return false, err
	}
	var won bool
	err = w.e.db.Update(ctx, func(tx store.Tx) error {
		if err := confirm(ctx, tx, held); err != nil || held.absent {
			return err
		}
		current, ok, err := readSpokeNativeRoute(ctx, tx, w.e.boot, wake.Route.Member)
		if err != nil || !ok || current != local || w.e.stop.halted.Load() {
			return err
		}
		cut, err := spool.ReserveLeafPublishSeq(ctx, held.dir, held.seq)
		if err != nil {
			return err
		}
		_, won, err = claimSpokeWake(ctx, tx, wake.AttemptID, wake.Route, cut)
		return err
	})
	return won && err == nil, err
}
