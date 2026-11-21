package dispatch

import (
	"context"
	"runtime"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/proc"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

const launchSweepInterval = time.Second
const launchSweepBatch = 32

type launchSweep struct {
	next    time.Time
	after   int64
	pending <-chan launchSweepResult
}

type launchSweepResult struct {
	after  int64
	probes []domain.ClaudeLaunchProbe
	err    error
}

func (s *launchSweep) start(ctx context.Context, db *store.Store, now time.Time) {
	if s.pending != nil || now.Before(s.next) {
		return
	}
	s.next = now.Add(launchSweepInterval)
	result := make(chan launchSweepResult, 1)
	s.pending = result
	go func(after int64) {
		result <- probeClaudeLaunches(ctx, db, after)
	}(s.after)
}

func probeClaudeLaunches(ctx context.Context, db *store.Store, after int64) launchSweepResult {
	var launches []domain.ClaudeLaunch
	err := db.Update(ctx, func(tx store.Tx) error {
		var err error
		launches, err = domain.LiveClaudeLaunches(ctx, tx, after, launchSweepBatch)
		return err
	})
	if err != nil {
		return launchSweepResult{after: after, err: err}
	}
	var result launchSweepResult
	if len(launches) == launchSweepBatch {
		result.after = launches[len(launches)-1].RowID
	}
	pidNamespace := proc.PIDNamespace()
	for _, launch := range launches {
		if err := ctx.Err(); err != nil {
			return launchSweepResult{after: after, err: err}
		}
		if !sameLaunchPIDNamespace(runtime.GOOS, pidNamespace, launch.PIDNamespace) {
			continue
		}
		if launch.ProcessPID != 0 {
			if proc.ProcessGone(launch.ProcessPID, launch.ProcessStartIdentity) {
				result.probes = append(result.probes, domain.ClaudeLaunchProbe{ClaudeLaunch: launch, Gone: true})
			}
			continue
		}
		pid, identity, gone := proc.ProbeLaunch(launch.LaunchRef, launch.StartedAtMS)
		if gone {
			result.probes = append(result.probes, domain.ClaudeLaunchProbe{ClaudeLaunch: launch, Gone: true})
		} else if identity != 0 {
			result.probes = append(result.probes, domain.ClaudeLaunchProbe{ClaudeLaunch: launch, PID: pid, StartIdentity: identity})
		}
	}
	return result
}

func sameLaunchPIDNamespace(platform, dispatcher, launch string) bool {
	if platform == "linux" {
		return dispatcher != "" && dispatcher == launch
	}
	return platform == "darwin" && launch == ""
}

func (r *launchSweep) apply(ctx context.Context, s *server, result launchSweepResult) error {
	r.pending = nil
	r.after = result.after
	if result.err != nil || len(result.probes) == 0 {
		return result.err
	}
	return s.db.Update(ctx, func(tx store.Tx) error {
		changed, err := domain.ApplyClaudeLaunchProbes(ctx, tx, result.probes, time.Now())
		if err != nil || !changed {
			return err
		}
		return s.applyAdapterStamps(ctx, tx)
	})
}
