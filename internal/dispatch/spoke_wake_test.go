package dispatch

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/link"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

func openSpokeWakeStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	root := t.TempDir()
	db, err := store.OpenSpoke(t.Context(), root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db, root
}

func testSpokeWake(attempt int64) link.Wake {
	return link.Wake{AttemptID: attempt, IDs: []int64{3, 5},
		Route: link.WakeRoute{Member: link.Member{Vendor: "claude", Key: "member"}, Boot: "boot", Launch: "launch",
			PolicyEpoch: 2, Lane: link.NewNonce(), Scope: link.MemberScope{
				LeafGeneration: "leaf", Membership: link.Membership{BindingID: 4, Generation: 2}, RouteEpoch: 7}}}
}

func TestSpokeWakeStagesWithTheCursor(t *testing.T) {
	f := newEventFixture(t)
	ctx := t.Context()
	wake := testSpokeWake(9)
	if _, err := f.e.applySnapshot(ctx, []snapshotPart{{Snapshot: link.Snapshot{Seq: 10, Last: true}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.applyEvent(ctx, event(t, 12, link.EventWake, wake), 0); err == nil {
		t.Fatal("a wake staged across a cursor gap")
	}
	if n := f.count(t, `SELECT COUNT(*) FROM spoke_wake_attempt`); n != 0 {
		t.Fatalf("%d wakes survived the cursor rollback", n)
	}
	if _, err := f.e.applyEvent(ctx, event(t, 11, link.EventWake, wake), 0); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM spoke_event_cursor, spoke_wake_attempt
		WHERE acked_seq = 11 AND attempt_id = 9 AND state = 'staged'`); n != 1 {
		t.Fatal("the wake and its cursor did not commit together")
	}
}

func TestSpokeWakeStreamReauthorizesOnlyAfterCatchUp(t *testing.T) {
	f := newEventFixture(t)
	f.allowWake = true
	wake := testSpokeWake(9)
	home := newFakeHome()
	f.start(t, home)
	home.push(t, link.TypeSnapshot, link.Snapshot{Seq: 10, Last: true})
	home.push(t, link.TypeEvent, event(t, 11, link.EventWake, wake))
	home.ack(t, 11)
	if n := f.count(t, `SELECT COUNT(*) FROM spoke_wake_attempt WHERE state = 'staged'`); n != 1 {
		t.Fatal("the wake was not staged before its ACK")
	}
	f.end(t, home)

	home = newFakeHome()
	f.start(t, home)
	home.push(t, link.TypeWakeReplay, wake)
	if err := f.end(t, home); err == nil || !strings.Contains(err.Error(), "catch-up") {
		t.Fatalf("early replay: %v", err)
	}
	if n := f.count(t, `SELECT COUNT(*) FROM spoke_wake_attempt WHERE state = 'dormant'`); n != 1 {
		t.Fatal("the acknowledged wake retained its authorization across disconnect")
	}

	home = newFakeHome()
	f.start(t, home)
	home.push(t, link.TypeCaughtUp, link.CaughtUp{Seq: 11})
	home.push(t, link.TypeWakeReplay, wake)
	home.push(t, link.TypeEvent, event(t, 12, link.EventWakeClosed, link.WakeClosed{AttemptID: 10}))
	home.ack(t, 12)
	if n := f.count(t, `SELECT COUNT(*) FROM spoke_wake_attempt WHERE attempt_id = 9 AND state = 'staged'`); n != 1 {
		t.Fatal("the replay did not reactivate the stored authorization")
	}
	home.push(t, link.TypeEvent, event(t, 13, link.EventWakeClosed, link.WakeClosed{AttemptID: 9}))
	home.ack(t, 13)
	f.end(t, home)
	if n := f.count(t, `SELECT COUNT(*) FROM spoke_wake_attempt WHERE state = 'closed'`); n != 2 {
		t.Fatalf("closed wakes = %d", n)
	}
}

func TestSpokeWakeStreamRejectsAnUnnegotiatedWake(t *testing.T) {
	f := newEventFixture(t)
	home := newFakeHome()
	f.start(t, home)
	home.push(t, link.TypeSnapshot, link.Snapshot{Seq: 10, Last: true})
	home.push(t, link.TypeEvent, event(t, 11, link.EventWake, testSpokeWake(9)))
	if err := f.end(t, home); err == nil || !strings.Contains(err.Error(), "without wake negotiation") {
		t.Fatalf("unnegotiated wake: %v", err)
	}
	if n := f.count(t, `SELECT acked_seq FROM spoke_event_cursor`); n != 10 {
		t.Fatalf("acknowledged unnegotiated wake: cursor %d", n)
	}
}

func TestSpokeWakeStreamRetriesUntilTheMatchingReceipt(t *testing.T) {
	f := newEventFixture(t)
	f.allowWake = true
	wake := testSpokeWake(9)
	ctx := t.Context()
	if err := f.m.db.Update(ctx, func(tx store.Tx) error {
		if err := stageSpokeWake(ctx, tx, wake); err != nil {
			return err
		}
		if _, won, err := claimSpokeWake(ctx, tx, wake.AttemptID, wake.Route, 7); err != nil || !won {
			t.Fatalf("claim = %t, %v", won, err)
		}
		return recoverSpokeWakeClaims(ctx, tx)
	}); err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		home := newFakeHome()
		f.start(t, home)
		from := uint64(10 + i)
		if i == 0 {
			home.push(t, link.TypeSnapshot, link.Snapshot{Seq: from, Last: true})
			home.ack(t, from)
			if len(home.frames()) != 2 {
				t.Fatal("result sent before catch-up")
			}
		}
		home.push(t, link.TypeCaughtUp, link.CaughtUp{Seq: from})
		if i == 1 {
			home.push(t, link.TypeWakeRecorded, link.WakeRecorded{AttemptID: wake.AttemptID, Status: link.RecordedApplied})
		}
		home.push(t, link.TypeEvent, event(t, from+1, link.EventWakeClosed, link.WakeClosed{AttemptID: 10}))
		home.ack(t, from+1)
		f.end(t, home)
		var results []link.WakeResult
		for _, frame := range home.frames() {
			if frame.Type == link.TypeWakeResult {
				var result link.WakeResult
				if err := json.Unmarshal(frame.Body, &result); err != nil {
					t.Fatal(err)
				}
				results = append(results, result)
			}
		}
		want := link.WakeResult{AttemptID: wake.AttemptID, Route: wake.Route, Outcome: link.WakeUncertain, SubmissionSeq: 7}
		if !slices.Equal(results, []link.WakeResult{want}) {
			t.Fatalf("stream %d sent %+v, want %+v", i+1, results, want)
		}
		if n := f.count(t, `SELECT recorded FROM spoke_wake_attempt WHERE attempt_id = 9`); n != i {
			t.Fatalf("stream %d recorded = %d", i+1, n)
		}
	}
}

func TestSpokeWakeStreamSendsAResultCommittedAfterCatchUp(t *testing.T) {
	f := newEventFixture(t)
	f.allowWake = true
	f.e.wakeResultReady = make(chan struct{}, 1)
	home := newFakeHome()
	f.start(t, home)
	home.push(t, link.TypeSnapshot, link.Snapshot{Seq: 10, Last: true})
	home.push(t, link.TypeCaughtUp, link.CaughtUp{Seq: 10})
	wake := testSpokeWake(9)
	home.push(t, link.TypeEvent, event(t, 11, link.EventWake, wake))
	home.ack(t, 11)

	ctx := t.Context()
	if changed, err := f.e.finishWake(ctx, wake.AttemptID, link.WakeSubmitted); changed || err != nil {
		t.Fatalf("unclaimed outcome changed the journal: %t, %v", changed, err)
	}
	if err := f.m.db.Update(ctx, func(tx store.Tx) error {
		if _, won, err := claimSpokeWake(ctx, tx, wake.AttemptID, wake.Route, 7); err != nil || !won {
			t.Fatalf("claim = %t, %v", won, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if changed, err := f.e.finishWake(ctx, wake.AttemptID, link.WakeSubmitted); !changed || err != nil {
		t.Fatalf("claimed outcome: %t, %v", changed, err)
	}
	want := link.WakeResult{AttemptID: wake.AttemptID, Route: wake.Route, Outcome: link.WakeSubmitted, SubmissionSeq: 7}
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		for _, frame := range home.frames() {
			if frame.Type != link.TypeWakeResult {
				continue
			}
			var got link.WakeResult
			if err := json.Unmarshal(frame.Body, &got); err != nil || got != want {
				t.Fatalf("live result = %+v, %v", got, err)
			}
			home.push(t, link.TypeWakeRecorded, link.WakeRecorded{AttemptID: wake.AttemptID, Status: link.RecordedApplied})
			home.push(t, link.TypeEvent, event(t, 12, link.EventWakeClosed, link.WakeClosed{AttemptID: 10}))
			home.ack(t, 12)
			f.end(t, home)
			if n := f.count(t, `SELECT recorded FROM spoke_wake_attempt WHERE attempt_id = 9`); n != 1 {
				t.Fatal("the live result receipt was not persisted")
			}
			return
		}
	}
	t.Fatal("the open stream did not send the committed outcome")
}

func TestSpokeWakeRequiresFreshAuthorizationAfterReconnect(t *testing.T) {
	db, _ := openSpokeWakeStore(t)
	ctx := t.Context()
	wake, closed := testSpokeWake(9), testSpokeWake(10)
	if err := db.Update(ctx, func(tx store.Tx) error {
		if err := stageSpokeWake(ctx, tx, wake); err != nil {
			return err
		}
		if err := suspendSpokeWakes(ctx, tx); err != nil {
			return err
		}
		if _, won, err := claimSpokeWake(ctx, tx, wake.AttemptID, wake.Route, 6); err != nil || won {
			t.Fatalf("dormant wake was claimable: %t, %v", won, err)
		}
		if err := closeSpokeWake(ctx, tx, closed.AttemptID); err != nil {
			return err
		}
		if err := stageSpokeWake(ctx, tx, closed); err != nil {
			return err
		}
		if _, won, err := claimSpokeWake(ctx, tx, closed.AttemptID, closed.Route, 6); err != nil || won {
			t.Fatalf("replay reopened a closed attempt: %t, %v", won, err)
		}
		return stageSpokeWake(ctx, tx, wake)
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Update(ctx, func(tx store.Tx) error {
		stale := wake.Route
		stale.Scope.Membership.Generation++
		if _, won, err := claimSpokeWake(ctx, tx, wake.AttemptID, stale, 7); err != nil || won {
			t.Fatalf("changed local scope won claim: %t, %v", won, err)
		}
		got, won, err := claimSpokeWake(ctx, tx, wake.AttemptID, wake.Route, 7)
		if err != nil || !won || got.Route != wake.Route || !slices.Equal(got.IDs, wake.IDs) {
			t.Fatalf("fresh claim = %+v, %t, %v", got, won, err)
		}
		if err := stageSpokeWake(ctx, tx, wake); err != nil {
			return err
		}
		if _, won, err := claimSpokeWake(ctx, tx, wake.AttemptID, wake.Route, 8); err != nil || won {
			t.Fatalf("duplicate wake won claim: %t, %v", won, err)
		}
		changed := wake
		changed.IDs = []int64{7}
		if err := stageSpokeWake(ctx, tx, changed); err == nil {
			t.Fatal("a duplicate rewrote the original wake")
		}
		return closeSpokeWake(ctx, tx, wake.AttemptID)
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSpokeWakeRecoversTheClaimAndReplaysItsResult(t *testing.T) {
	db, root := openSpokeWakeStore(t)
	ctx := t.Context()
	wake := testSpokeWake(9)
	if err := db.Update(ctx, func(tx store.Tx) error {
		if err := stageSpokeWake(ctx, tx, wake); err != nil {
			return err
		}
		if _, won, err := claimSpokeWake(ctx, tx, wake.AttemptID, wake.Route, 8); err != nil || !won {
			t.Fatalf("claim = %t, %v", won, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := store.OpenSpoke(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Update(ctx, func(tx store.Tx) error {
		if err := recoverSpokeWakeClaims(ctx, tx); err != nil {
			return err
		}
		if err := stageSpokeWake(ctx, tx, wake); err != nil {
			return err
		}
		if _, won, err := claimSpokeWake(ctx, tx, wake.AttemptID, wake.Route, 9); err != nil || won {
			t.Fatalf("recovered claim could execute twice: %t, %v", won, err)
		}
		first, err := nextSpokeWakeResult(ctx, tx)
		if err != nil || first == nil || first.AttemptID != wake.AttemptID ||
			first.Route != wake.Route || first.Outcome != link.WakeUncertain || first.SubmissionSeq != 8 {
			t.Fatalf("recovered result = %+v, %v", first, err)
		}
		if err := recordSpokeWakeReceipt(ctx, tx, wake.AttemptID, link.WakeRecorded{
			AttemptID: wake.AttemptID + 1, Status: link.RecordedApplied}); err == nil {
			t.Fatal("an unrelated receipt matched the result")
		}
		again, err := nextSpokeWakeResult(ctx, tx)
		if err != nil || again == nil || *again != *first {
			t.Fatalf("retry changed the durable result: %+v, %v", again, err)
		}
		return recordSpokeWakeReceipt(ctx, tx, wake.AttemptID, link.WakeRecorded{
			AttemptID: wake.AttemptID, Status: link.RecordedStale})
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.View(ctx, func(tx store.Tx) error {
		result, err := nextSpokeWakeResult(ctx, tx)
		if err == nil && result != nil {
			t.Fatalf("acknowledged result would be sent again: %+v", result)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestSpokeWakeStoresKnownOutcomes(t *testing.T) {
	for _, outcome := range []string{link.WakeNotSubmitted, link.WakeSubmitted} {
		t.Run(outcome, func(t *testing.T) {
			db, _ := openSpokeWakeStore(t)
			ctx := t.Context()
			const attempt = 9
			wake := testSpokeWake(attempt)
			if err := db.Update(ctx, func(tx store.Tx) error {
				if err := stageSpokeWake(ctx, tx, wake); err != nil {
					return err
				}
				var cut uint64
				if outcome == link.WakeSubmitted {
					cut = 3
					if _, won, err := claimSpokeWake(ctx, tx, attempt, wake.Route, cut); err != nil || !won {
						t.Fatalf("claim = %t, %v", won, err)
					}
				}
				if changed, err := finishSpokeWake(ctx, tx, attempt, outcome); !changed || err != nil {
					t.Fatalf("outcome update = %t, %v", changed, err)
				}
				if _, won, err := claimSpokeWake(ctx, tx, attempt, wake.Route, 4); err != nil || won {
					t.Fatalf("finished wake could execute: %t, %v", won, err)
				}
				got, err := nextSpokeWakeResult(ctx, tx)
				if err != nil || got == nil || got.Outcome != outcome || got.SubmissionSeq != cut {
					t.Fatalf("stored outcome = %+v, %v", got, err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
