package dispatch

import (
	"context"
	"runtime"
	"strings"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/proc"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

const launchSweepInterval = time.Second
const launchSweepBatch = 32

type launchSweep struct {
	next      time.Time
	after     int64
	afterHost int64
	pending   <-chan launchSweepResult
}

type launchSweepResult struct {
	after  int64
	probes []domain.ClaudeLaunchProbe
	// afterHost and goneHosts are the same for members a host tracks.
	afterHost int64
	goneHosts []domain.HostedMember
	err       error
}

func (s *launchSweep) start(ctx context.Context, db *store.Store, now time.Time) {
	if s.pending != nil || now.Before(s.next) {
		return
	}
	s.next = now.Add(launchSweepInterval)
	result := make(chan launchSweepResult, 1)
	s.pending = result
	go func(after, afterHost int64) {
		r := probeClaudeLaunches(ctx, db, after)
		if r.err == nil {
			r.afterHost, r.goneHosts, r.err = probeMemberHosts(ctx, db, afterHost)
		}
		result <- r
	}(s.after, s.afterHost)
}

// probeMemberHosts returns the members, of one batch after afterHost, whose
// host is proven gone, and where the next batch starts.
func probeMemberHosts(ctx context.Context, db *store.Store, afterHost int64) (int64, []domain.HostedMember, error) {
	var members []domain.HostedMember
	err := db.Update(ctx, func(tx store.Tx) error {
		var err error
		members, err = domain.HostedMembers(ctx, tx, afterHost, launchSweepBatch)
		return err
	})
	if err != nil {
		return afterHost, nil, err
	}
	next := int64(0)
	if len(members) == launchSweepBatch {
		next = members[len(members)-1].PrincipalID
	}
	pidNamespace := proc.PIDNamespace()
	var gone []domain.HostedMember
	for _, m := range members {
		if err := ctx.Err(); err != nil {
			return afterHost, nil, err
		}
		sameNamespace, previousBoot := compareLaunchPIDNamespaces(runtime.GOOS, pidNamespace, m.PIDNamespace)
		if previousBoot || sameNamespace && proc.ProcessGone(m.PID, m.StartIdentity) {
			gone = append(gone, m)
		}
	}
	return next, gone, nil
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
		sameNamespace, previousBoot := compareLaunchPIDNamespaces(runtime.GOOS, pidNamespace, launch.PIDNamespace)
		if previousBoot {
			result.probes = append(result.probes, domain.ClaudeLaunchProbe{ClaudeLaunch: launch, Gone: true, PreviousBoot: true})
			continue
		}
		if !sameNamespace {
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

func compareLaunchPIDNamespaces(platform, dispatcher, launch string) (sameNamespace, previousBoot bool) {
	if platform != "linux" {
		return platform == "darwin" && launch == "", false
	}
	dispatcherBoot, launchBoot := proc.PIDNamespaceBoot(dispatcher), proc.PIDNamespaceBoot(launch)
	if dispatcherBoot == "" || launchBoot == "" {
		return false, false
	}
	_, dispatcherInode, _ := strings.Cut(dispatcher, ":")
	_, launchInode, _ := strings.Cut(launch, ":")
	// A container can report a different boot ID while the host still runs.
	return dispatcher == launch, dispatcherBoot != launchBoot && dispatcherInode == launchInode
}

func (r *launchSweep) apply(ctx context.Context, s *server, result launchSweepResult) error {
	r.pending = nil
	r.after, r.afterHost = result.after, result.afterHost
	if result.err != nil || len(result.probes) == 0 && len(result.goneHosts) == 0 {
		return result.err
	}
	return s.db.Update(ctx, func(tx store.Tx) error {
		now := time.Now()
		changed, err := domain.ApplyClaudeLaunchProbes(ctx, tx, result.probes, now)
		if err != nil {
			return err
		}
		departed, err := domain.DepartGoneHosts(ctx, tx, result.goneHosts, now)
		if err != nil || !changed && !departed {
			return err
		}
		return s.applyAdapterStamps(ctx, tx)
	})
}
