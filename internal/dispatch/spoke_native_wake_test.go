package dispatch

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/link"
	"github.com/BeomSeogKim/crosshall/internal/proc"
	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

func TestSpokeNativeWakeNeedsFreshAuthorization(t *testing.T) {
	var sends, proofs atomic.Int32
	queue := publish.Queue{PID: 19, StartMS: 29}
	t.Cleanup(publish.Swap(publish.Shipped().WithTransport("codex", publish.Transport{
		Prove: func(_ context.Context, probe publish.Probe) (publish.Proven, bool) {
			proofs.Add(1)
			return publish.Proven{Queue: queue, StartedAtMS: 10}, probe.Route.Host == queue
		},
		Send: func(_ context.Context, _ publish.Target, _ string) publish.Outcome {
			sends.Add(1)
			return publish.Submitted
		},
	})))
	f := newHookFixture(t)
	f.end(t, f.home)
	if err := f.dir.WriteFile("hook-publish-seq", []byte("7\n"), 0600); err != nil {
		t.Fatal(err)
	}
	args := wire.HookEnrollArgs{Profile: wire.HookProfile{Adapter: "codex", Mode: publish.ModeDaemon},
		LaunchRef: hookLaunch, Host: &wire.Host{PID: queue.PID, Start: queue.StartMS, NS: proc.PIDNamespace()},
		VendorHome: t.TempDir(), HookAtMS: 20, EventRef: "start", Epoch: 3}
	if result := f.hook(t, wire.VerbHookEnroll, args, 7, stamp22); result.Status != core.ExitOK {
		t.Fatalf("route capture: %+v", result)
	}
	f.exec(t, `UPDATE spoke_offer SET suppressed = 1`)
	worker := spokeWakeWorker{e: f.e, stateRoot: t.TempDir()}
	if routes, err := worker.readyRoutes(t.Context()); err != nil || len(routes) != 0 || proofs.Load() != 0 {
		t.Fatalf("idle route poll: %+v, %v, %d proofs", routes, err, proofs.Load())
	}
	f.exec(t, `UPDATE spoke_offer SET suppressed = 0`)
	f.e.nativeWakeRoot, f.e.nativeEnrollment = t.TempDir(), "test-link"
	f.e.wakeResultReady = make(chan struct{}, 1)
	f.allowWake = true
	home := newFakeHome()
	f.start(t, home)
	defer f.end(t, home)
	home.push(t, link.TypeCaughtUp, link.CaughtUp{Seq: 10})
	var route link.WakeRoute
	waitNativeWakeFrame(t, home, link.TypeWakeReady, &route)
	wake := link.Wake{AttemptID: 19, Route: route, IDs: []int64{42}}
	home.push(t, link.TypeEvent, event(t, 11, link.EventWake, wake))
	home.ack(t, 11)
	if n := f.count(t, `SELECT COUNT(*) FROM spoke_wake_attempt WHERE attempt_id = 19 AND state = 'staged'`); n != 1 || sends.Load() != 0 {
		t.Fatalf("sequenced authorization = %d staged, %d sends", n, sends.Load())
	}
	home.push(t, link.TypeWakeReplay, wake)
	var result link.WakeResult
	waitNativeWakeFrame(t, home, link.TypeWakeResult, &result)
	if result.AttemptID != wake.AttemptID || result.Outcome != link.WakeSubmitted || result.SubmissionSeq != 8 || sends.Load() != 1 {
		t.Fatalf("fresh authorization: %+v, %d sends", result, sends.Load())
	}
	home.push(t, link.TypeWakeRecorded, link.WakeRecorded{AttemptID: wake.AttemptID, Status: link.RecordedApplied})
	home.push(t, link.TypeWakeReplay, wake)
	home.push(t, link.TypeEvent, event(t, 12, link.EventWakeClosed, link.WakeClosed{AttemptID: 20}))
	home.ack(t, 12)
	if n := f.count(t, `SELECT recorded FROM spoke_wake_attempt WHERE attempt_id = 19`); n != 1 || sends.Load() != 1 {
		t.Fatalf("repeat authorization = %d recorded, %d sends", n, sends.Load())
	}
}

func waitNativeWakeFrame(t *testing.T, home *fakeHome, kind string, out any) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		for _, frame := range home.frames() {
			if frame.Type == kind {
				if err := json.Unmarshal(frame.Body, out); err != nil {
					t.Fatal(err)
				}
				return
			}
		}
	}
	t.Fatalf("no %s frame", kind)
}
