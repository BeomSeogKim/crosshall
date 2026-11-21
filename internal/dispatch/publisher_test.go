package dispatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/notice"
	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/spool"
	"github.com/BeomSeogKim/crosshall/internal/store"
	"github.com/BeomSeogKim/crosshall/internal/wire"
)

var publishTestTarget = domain.PublishTarget{
	Adapter: "claude",
	Address: "/unused/claude.sock",
	Queue:   domain.PublishQueue{PID: 2345, StartMS: 1234},
}

type publishTestFixture struct {
	root string
	db   *store.Store
	now  time.Time
}

func newPublishTestFixture(t *testing.T) *publishTestFixture {
	t.Helper()
	// Claude can publish; a test that sends gives it its own transport.
	useTransport(t, publish.Transport{Send: func(context.Context, domain.PublishTarget, string) domain.PublishOutcome {
		return domain.PublishNotSubmitted
	}})
	f := &publishTestFixture{root: filepath.Join(t.TempDir(), "state"), now: time.Now()}
	if err := os.Mkdir(f.root, 0700); err != nil {
		t.Fatal(err)
	}
	f.open(t)
	t.Cleanup(func() { f.db.Close() })
	f.exec(t, `INSERT INTO channel(id, name, created_at_ms) VALUES (1, 'publish', 1);
		INSERT INTO principal(id, kind, vendor, created_at_ms)
			VALUES (1, 'session', 'codex', 1), (2, 'session', 'claude', 1);
		INSERT INTO binding(id, principal_id, channel_id, alias, joined_at_ms) VALUES (2, 2, 1, 'worker', 1);
		INSERT INTO adapter_policy(adapter, enabled, reminder, epoch, changed_at_ms) VALUES ('claude', 1, 1, 1, 1);
		INSERT INTO adapter_launch(principal_id, launch_ref, profile, verified, started_hook_at_ms)
			VALUES (2, 'launch', '{"vendor_version":"2.1.0","platform":"darwin-arm64","mode":"default","manifest_rev":"r1"}', 1, 1);
		INSERT INTO capability_evidence(vendor, vendor_version, platform, mode, manifest_rev, cap, level, result_ref, checked_at_ms)
			VALUES ('claude', '2.1.0', 'darwin-arm64', 'default', 'r1', 'publish', 'tested', 'publish-test', 1);
		INSERT INTO publish_route(binding_id, principal_id, generation, adapter, session_ref, launch_ref,
			launch_verified, policy_epoch, mode, socket_path, watermark_message_id, state, registered_at_ms, updated_at_ms)
			VALUES (2, 2, 3, 'claude', 'session', 'launch', 1, 1, 'tui', '/unused/claude.sock', 10, 'active', 1, 1);
		INSERT INTO publish_route_proof(binding_id, generation, process_pid, process_start_ms, activated_at_ms)
			VALUES (2, 3, 2345, 1234, 1)`)
	return f
}

func (f *publishTestFixture) open(t *testing.T) {
	t.Helper()
	db, err := store.Open(t.Context(), f.root)
	if err != nil {
		t.Fatal(err)
	}
	f.db = db
}

func (f *publishTestFixture) update(t *testing.T, apply func(store.Tx) error) {
	t.Helper()
	if err := f.db.Update(t.Context(), apply); err != nil {
		t.Fatal(err)
	}
}

func (f *publishTestFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	f.update(t, func(tx store.Tx) error {
		_, err := tx.ExecContext(t.Context(), query, args...)
		return err
	})
}

func (f *publishTestFixture) ask(t *testing.T, id int64) {
	t.Helper()
	f.exec(t, `INSERT INTO message(id, channel_id, sender_id, kind, audience_mode, body, created_at_ms)
			VALUES (?1, 1, 1, 'ask', 'explicit', 'private request body', 1);
		INSERT INTO recipient(message_id, principal_id, alias_at_commit, owes) VALUES (?1, 2, 'worker', 1);
		INSERT INTO obligation(message_id, principal_id) VALUES (?1, 2)`, id)
}

func (f *publishTestFixture) reserve(t *testing.T, submitting bool) int64 {
	t.Helper()
	var id int64
	f.update(t, func(tx store.Tx) error {
		ctx := t.Context()
		if ok, err := domain.ReservePublish(ctx, tx, domain.PublishBinding{ID: 2, Generation: 3}, f.now); err != nil || !ok {
			return fmt.Errorf("reserve fixture: ok=%t, err=%v", ok, err)
		}
		ids, err := domain.ReservedPublishAttempts(ctx, tx)
		if err != nil || len(ids) != 1 {
			return fmt.Errorf("reserved fixture: ids=%v, err=%v", ids, err)
		}
		id = ids[0]
		if submitting {
			if _, items, err := domain.BeginPublish(ctx, tx, id, f.now); err != nil || len(items) != 1 {
				return fmt.Errorf("begin fixture: items=%v, err=%v", items, err)
			}
		}
		return nil
	})
	return id
}

func (f *publishTestFixture) requireAttempt(t *testing.T, id int64, want domain.PublishOutcome) {
	t.Helper()
	var state domain.PublishOutcome
	var count int
	f.update(t, func(tx store.Tx) error {
		ctx := t.Context()
		if err := tx.QueryRowContext(ctx, "SELECT state FROM publish_attempt WHERE id = ?", id).Scan(&state); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM publish_attempt").Scan(&count)
	})
	if state != want || count != 1 {
		t.Fatalf("attempt %d state=%s, attempts=%d, want %s and one attempt", id, state, count, want)
	}
}

type publishTestCall struct {
	ctx    context.Context
	target domain.PublishTarget
	line   string
}

func (f *publishTestFixture) publisher(t *testing.T, gate <-chan struct{}) (*publisher, <-chan publishTestCall) {
	t.Helper()
	calls := make(chan publishTestCall, 8)
	useTransport(t, publish.Transport{Send: func(ctx context.Context, target domain.PublishTarget, line string) domain.PublishOutcome {
		calls <- publishTestCall{ctx, target, line}
		if gate != nil {
			select {
			case <-ctx.Done():
				return domain.PublishUncertain
			case <-gate:
			}
		}
		return domain.PublishSubmitted
	}})
	p := &publisher{
		db: f.db,
	}
	t.Cleanup(p.workers.Wait)
	return p, calls
}

func tickPublisher(t *testing.T, p *publisher, now time.Time, startup bool) {
	t.Helper()
	if err := p.tick(t.Context(), now, startup); err != nil {
		t.Fatal(err)
	}
}

func awaitPublishTest[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(4 * time.Second):
		t.Fatal("timed out waiting for publisher")
		var zero T
		return zero
	}
}

func requirePublished(t *testing.T, call publishTestCall, ids ...int64) {
	t.Helper()
	if call.target != publishTestTarget || call.line != claudeNotice(ids) {
		t.Fatalf("published to %+v with %q, want %+v with IDs %v", call.target, call.line, publishTestTarget, ids)
	}
}

func requireNoPublish(t *testing.T, calls <-chan publishTestCall) {
	t.Helper()
	select {
	case call := <-calls:
		t.Fatalf("unexpected publish to %+v: %q", call.target, call.line)
	default:
	}
}

func publishWorkersDone(p *publisher) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		p.workers.Wait()
		close(done)
	}()
	return done
}

func TestPublisherStartupPicksUpStoredWork(t *testing.T) {
	f := newPublishTestFixture(t)
	f.ask(t, 10)
	principal := int64(2)
	f.update(t, func(tx store.Tx) error {
		svc := &domain.Service{}
		result, err := svc.Handle(t.Context(), tx, core.Caller{Role: core.RoleMember, PrincipalID: &principal},
			domain.Request{Command: domain.Wait{WorkOnly: true}})
		if err != nil {
			return err
		}
		if result.Status != core.ExitOK {
			return fmt.Errorf("initial waiter snapshot: %+v", result)
		}
		return nil
	})
	// The waiter disappears after this snapshot; the publisher has no lease to clear.
	f.ask(t, 11)
	p, calls := f.publisher(t, nil)
	if err := p.recover(t.Context(), f.now); err != nil {
		t.Fatal(err)
	}
	tickPublisher(t, p, f.now, true)
	p.workers.Wait()
	requirePublished(t, awaitPublishTest(t, calls), 11)
	f.requireAttempt(t, 1, domain.PublishSubmitted)
	tickPublisher(t, p, f.now.Add(time.Second), false)
	p.workers.Wait()
	requireNoPublish(t, calls)
}

func TestPublisherCoalescesAndRereadsUnreadWork(t *testing.T) {
	f := newPublishTestFixture(t)
	f.ask(t, 11)
	f.ask(t, 12)
	p, calls := f.publisher(t, nil)
	tickPublisher(t, p, f.now, false)
	p.workers.Wait()
	requireNoPublish(t, calls)
	f.exec(t, "UPDATE recipient SET picked_up_at_ms = 2 WHERE message_id = 11")
	f.ask(t, 13)
	tickPublisher(t, p, f.now.Add(999*time.Millisecond), false)
	p.workers.Wait()
	requireNoPublish(t, calls)
	tickPublisher(t, p, f.now.Add(time.Second), false)
	p.workers.Wait()
	requirePublished(t, awaitPublishTest(t, calls), 12, 13)
	f.requireAttempt(t, 1, domain.PublishSubmitted)
	requireNoPublish(t, calls)
}

func TestPublisherStartsFreshCoalescingWindowOnlyAfterDrainingBatch(t *testing.T) {
	for _, overflow := range []bool{false, true} {
		t.Run(fmt.Sprintf("overflow=%t", overflow), func(t *testing.T) {
			f := newPublishTestFixture(t)
			var first []int64
			for id := int64(11); id < 11+notice.OfferCap; id++ {
				f.ask(t, id)
				first = append(first, id)
			}
			nextID := int64(11 + notice.OfferCap)
			if overflow {
				f.ask(t, nextID)
			}
			p, calls := f.publisher(t, nil)
			capture := p.transport("claude").Send
			setSend(t, func(ctx context.Context, target domain.PublishTarget, line string) domain.PublishOutcome {
				capture(ctx, target, line)
				return domain.PublishNotSubmitted
			})
			tickPublisher(t, p, f.now, false)
			tickPublisher(t, p, f.now.Add(time.Second), false)
			p.workers.Wait()
			requirePublished(t, awaitPublishTest(t, calls), first...)
			requireNoPublish(t, calls)
			f.requireAttempt(t, 1, domain.PublishNotSubmitted)

			nextTick := f.now.Add(1250 * time.Millisecond)
			if !overflow {
				f.ask(t, nextID)
				tickPublisher(t, p, nextTick, false)
				tickPublisher(t, p, nextTick.Add(999*time.Millisecond), false)
				p.workers.Wait()
				requireNoPublish(t, calls)
				nextTick = nextTick.Add(time.Second)
			}
			tickPublisher(t, p, nextTick, false)
			p.workers.Wait()
			requirePublished(t, awaitPublishTest(t, calls), nextID)
			requireNoPublish(t, calls)
		})
	}
}

func TestPublisherRestartRecoversDurableAttempt(t *testing.T) {
	for _, test := range []struct {
		name       string
		submitting bool
		wrote      bool
		want       domain.PublishOutcome
	}{
		{"reserved", false, false, domain.PublishSubmitted},
		{"before-io", true, false, domain.PublishUncertain},
		{"after-io-before-record", false, true, domain.PublishUncertain},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newPublishTestFixture(t)
			f.ask(t, 11)
			id := f.reserve(t, test.submitting)
			if test.wrote {
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				result := make(chan error, 1)
				var call publishTestCall
				useTransport(t, publish.Transport{Send: func(ctx context.Context, target domain.PublishTarget, line string) domain.PublishOutcome {
					call = publishTestCall{ctx, target, line}
					result <- f.db.Close()
					cancel()
					return domain.PublishSubmitted
				}})
				p := &publisher{db: f.db}
				p.submit(ctx, id)
				if err := awaitPublishTest(t, result); err != nil {
					t.Fatal(err)
				}
				requirePublished(t, call, 11)
			} else if err := f.db.Close(); err != nil {
				t.Fatal(err)
			}
			f.open(t)
			f.ask(t, 12)
			p, calls := f.publisher(t, nil)
			if err := p.recover(t.Context(), f.now); err != nil {
				t.Fatal(err)
			}
			tickPublisher(t, p, f.now, true)
			p.workers.Wait()
			if !test.submitting && !test.wrote {
				requirePublished(t, awaitPublishTest(t, calls), 11)
			}
			tickPublisher(t, p, f.now.Add(time.Second), false)
			p.workers.Wait()
			requireNoPublish(t, calls)
			f.requireAttempt(t, id, test.want)
		})
	}
}

func TestPublisherCompetingWorkersSubmitOnce(t *testing.T) {
	f := newPublishTestFixture(t)
	f.ask(t, 11)
	id := f.reserve(t, false)
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	first, firstCalls := f.publisher(t, gate)
	second, secondCalls := f.publisher(t, gate)
	start := make(chan struct{})
	finished := make(chan struct{}, 2)
	for _, p := range []*publisher{first, second} {
		p.workers.Go(func() {
			<-start
			p.submit(t.Context(), id)
			finished <- struct{}{}
		})
	}
	t.Cleanup(release)
	close(start)
	awaitPublishTest(t, finished)
	f.requireAttempt(t, id, "submitting")
	release()
	first.workers.Wait()
	second.workers.Wait()
	if len(firstCalls)+len(secondCalls) != 1 {
		t.Fatalf("transport calls=%d+%d, want one total", len(firstCalls), len(secondCalls))
	}
	f.requireAttempt(t, id, domain.PublishSubmitted)
}

func TestPublisherTransportCancellationWaitsForCallbackExit(t *testing.T) {
	for _, test := range []struct {
		name   string
		retire bool
		want   error
	}{
		{"send timeout", false, context.DeadlineExceeded},
		{"send shutdown", false, context.Canceled},
		{"retirement timeout", true, context.DeadlineExceeded},
		{"retirement shutdown", true, context.Canceled},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newPublishTestFixture(t)
			f.ask(t, 11)
			started, exited := make(chan struct{}), make(chan struct{})
			canceled := make(chan error, 1)
			cleanup := make(chan struct{})
			finish := sync.OnceFunc(func() { close(cleanup) })
			defer finish()
			run := func(ctx context.Context) {
				close(started)
				<-ctx.Done()
				canceled <- ctx.Err()
				<-cleanup
				close(exited)
			}
			p := &publisher{db: f.db}
			if test.retire {
				f.reserve(t, true)
				if err := p.recover(t.Context(), f.now); err != nil {
					t.Fatal(err)
				}
				setRetired(t, func(ctx context.Context, _ domain.PublishQueue) bool {
					run(ctx)
					return true
				})
			} else {
				setSend(t, func(ctx context.Context, _ domain.PublishTarget, _ string) domain.PublishOutcome {
					run(ctx)
					return domain.PublishSubmitted
				})
			}
			t.Cleanup(p.workers.Wait)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if err := p.tick(ctx, f.now, true); err != nil {
				t.Fatal(err)
			}
			awaitPublishTest(t, started)
			if test.want == context.Canceled {
				cancel()
			}
			if got := awaitPublishTest(t, canceled); got != test.want {
				t.Fatalf("callback context error=%v, want %v", got, test.want)
			}
			done := publishWorkersDone(p)
			select {
			case <-done:
				t.Fatal("worker finished before its callback exited")
			case <-time.After(50 * time.Millisecond):
			}
			finish()
			awaitPublishTest(t, done)
			awaitPublishTest(t, exited)
			f.requireAttempt(t, 1, domain.PublishUncertain)
			var queues []domain.HeldPublishQueue
			f.update(t, func(tx store.Tx) error {
				var err error
				queues, err = domain.HeldPublishQueues(t.Context(), tx)
				return err
			})
			if !slices.Equal(queues, []domain.HeldPublishQueue{{Adapter: "claude", Queue: publishTestTarget.Queue}}) {
				t.Fatalf("held queues=%v, want %v", queues, publishTestTarget.Queue)
			}
		})
	}
}

type publishLockLog struct {
	io.Writer
	locked chan struct{}
}

func (w publishLockLog) Write(raw []byte) (int, error) {
	line := string(raw)
	if strings.Contains(line, "dispatcher: publish:") && strings.Contains(line, "database is locked") {
		select {
		case w.locked <- struct{}{}:
		default:
		}
	}
	return w.Writer.Write(raw)
}

func TestPublisherRetriesOutcomeWriteWithoutResending(t *testing.T) {
	f := newPublishTestFixture(t)
	f.ask(t, 11)
	locker, err := sql.Open("sqlite", filepath.Join(f.root, "store.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Close()
	ctx := t.Context()
	conn, err := locker.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	locked := make(chan struct{}, 1)
	previous := log.Writer()
	log.SetOutput(publishLockLog{Writer: previous, locked: locked})
	defer log.SetOutput(previous)
	calls := make(chan publishTestCall, 2)
	returnOutcome := make(chan struct{})
	useTransport(t, publish.Transport{
		Send: func(ctx context.Context, target domain.PublishTarget, line string) domain.PublishOutcome {
			calls <- publishTestCall{ctx, target, line}
			select {
			case <-ctx.Done():
				return domain.PublishUncertain
			case <-returnOutcome:
				return domain.PublishNotSubmitted
			}
		},
	})
	p := &publisher{db: f.db}
	t.Cleanup(p.workers.Wait)
	tickPublisher(t, p, f.now, true)
	requirePublished(t, awaitPublishTest(t, calls), 11)
	if _, err := conn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	defer conn.ExecContext(context.Background(), "ROLLBACK")
	close(returnOutcome)
	awaitPublishTest(t, locked)
	var state string
	if err := conn.QueryRowContext(ctx, "SELECT state FROM publish_attempt WHERE id = 1").Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != "submitting" {
		t.Fatalf("attempt while outcome write was locked = %s, want submitting", state)
	}
	if _, err := conn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	awaitPublishTest(t, publishWorkersDone(p))
	f.requireAttempt(t, 1, domain.PublishNotSubmitted)
	f.update(t, func(tx store.Tx) error {
		var claims int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM announcement").Scan(&claims); err != nil {
			return err
		}
		if claims != 0 {
			return fmt.Errorf("proven non-submission left %d announcement claims", claims)
		}
		return nil
	})
	tickPublisher(t, p, f.now.Add(time.Second), false)
	p.workers.Wait()
	requireNoPublish(t, calls)
}

func TestPublisherBlockedTransportLeavesScanAndTickResponsive(t *testing.T) {
	f := newPublishTestFixture(t)
	f.ask(t, 11)
	gate := make(chan struct{})
	release := sync.OnceFunc(func() { close(gate) })
	p, calls := f.publisher(t, gate)
	t.Cleanup(release)
	state, err := spool.OpenServerRoot(f.root)
	if err != nil {
		t.Fatal(err)
	}
	defer state.Close()
	cfg := Config{
		LaunchConfig: LaunchConfig{StateRoot: f.root, Rendezvous: filepath.Join(t.TempDir(), "rendezvous")},
		Services:     testServices(),
	}
	s := server{cfg: cfg, state: state, db: f.db, ready: readyMarker{Instance: "publish-test"}}
	defer s.withdrawReady()
	if err := prepareNamedSpool(state, "owner"); err != nil {
		t.Fatal(err)
	}
	tickPublisher(t, p, f.now, true)
	call := awaitPublishTest(t, calls)
	requirePublished(t, call, 11)
	owner := filepath.Join(f.root, "owner")
	request := wire.Request{
		Version: wire.Version, Attempt: "1234567890abcdef1234567890abcdef",
		OpKey: "publish-scan", Verb: "owner channel create", Args: json.RawMessage(`{"name":"checkout"}`),
		IssuedAtMS: f.now.UnixMilli(), ExpiresAtMS: f.now.Add(10 * time.Second).UnixMilli(),
	}
	writePacket(t, filepath.Join(owner, "in"), request)
	if err := s.scan(t.Context()); err != nil {
		t.Fatal(err)
	}
	tickPublisher(t, p, f.now.Add(time.Second), false)
	if response := awaitResponse(t, owner, request.Attempt); response.Status != core.ExitOK {
		t.Fatalf("scan response: %+v", response)
	}
	if err := call.ctx.Err(); err != nil {
		t.Fatalf("transport stopped awaiting its result before scan and tick completed: %v", err)
	}
	f.requireAttempt(t, 1, "submitting")
	release()
	p.workers.Wait()
	f.requireAttempt(t, 1, domain.PublishSubmitted)
	requireNoPublish(t, calls)
}

func TestPublisherDeadlineHoldsWhenTheTransportIgnoresCancellation(t *testing.T) {
	for _, retire := range []bool{false, true} {
		t.Run(fmt.Sprintf("retire=%t", retire), func(t *testing.T) {
			f := newPublishTestFixture(t)
			f.ask(t, 11)
			stuck := make(chan struct{})
			defer close(stuck)
			started := make(chan struct{}, 1)
			p := &publisher{db: f.db, timeout: 50 * time.Millisecond, abandon: 50 * time.Millisecond}
			if retire {
				f.reserve(t, true)
				if err := p.recover(t.Context(), f.now); err != nil {
					t.Fatal(err)
				}
				setRetired(t, func(context.Context, domain.PublishQueue) bool {
					started <- struct{}{}
					<-stuck // ignores its context
					return true
				})
			} else {
				setSend(t, func(context.Context, domain.PublishTarget, string) domain.PublishOutcome {
					started <- struct{}{}
					<-stuck // ignores its context
					return domain.PublishSubmitted
				})
			}
			begun := time.Now()
			tickPublisher(t, p, f.now, true)
			awaitPublishTest(t, started)
			awaitPublishTest(t, publishWorkersDone(p))
			if elapsed := time.Since(begun); elapsed > 2*time.Second {
				t.Fatalf("worker took %v past a 100ms bound", elapsed)
			}
			f.requireAttempt(t, 1, domain.PublishUncertain)
			var queues []domain.HeldPublishQueue
			f.update(t, func(tx store.Tx) error {
				var err error
				queues, err = domain.HeldPublishQueues(t.Context(), tx)
				return err
			})
			if len(queues) != 1 {
				t.Fatalf("held queues %v: an unproven retirement released the lane", queues)
			}
		})
	}
}

func TestPublisherStartupSeedsTheShippedEvidence(t *testing.T) {
	f := newPublishTestFixture(t)
	p := &publisher{db: f.db, evidence: []domain.BuiltinEvidence{{Vendor: "claude", Version: "9.9.9", Platform: "darwin-arm64",
		Mode: "print", ManifestRev: "r1", Capability: "turn_end", ResultRef: "sha256:bb", CheckedAtMS: 7}}}
	if err := p.recover(t.Context(), f.now); err != nil {
		t.Fatal(err)
	}
	var n int
	f.update(t, func(tx store.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM capability_evidence WHERE cap = 'turn_end'
			AND level = 'tested' AND source = 'builtin' AND checked_at_ms = 7 AND vendor_version = '9.9.9'`).Scan(&n)
	})
	if n != 1 {
		t.Fatalf("shipped profile rows %d, want 1", n)
	}
}

func TestPublisherKeepsOneProbePerQueueWhileACallbackIsStuck(t *testing.T) {
	f := newPublishTestFixture(t)
	f.ask(t, 11)
	f.reserve(t, true)
	p := &publisher{db: f.db, timeout: 10 * time.Millisecond, abandon: 10 * time.Millisecond}
	if err := p.recover(t.Context(), f.now); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	stuck := make(chan struct{})
	release := sync.OnceFunc(func() { close(stuck) })
	defer release()
	setRetired(t, func(context.Context, domain.PublishQueue) bool {
		calls.Add(1)
		<-stuck // ignores its context
		return false
	})
	for i := range 5 {
		tickPublisher(t, p, f.now.Add(time.Duration(i)*time.Second), false)
		awaitPublishTest(t, publishWorkersDone(p))
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("%d probes for one queue while the first is stuck, want 1", n)
	}
	release()
	deadline := time.Now().Add(5 * time.Second)
	for calls.Load() < 2 && time.Now().Before(deadline) {
		tickPublisher(t, p, f.now.Add(time.Minute), false)
		awaitPublishTest(t, publishWorkersDone(p))
		time.Sleep(5 * time.Millisecond)
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("after the callback returned: %d probes, want a new one", n)
	}
}

// useTransport gives claude t in the effective registry for this test.
func useTransport(t *testing.T, tr publish.Transport) {
	t.Helper()
	t.Cleanup(publish.Swap(publish.Shipped().WithTransport("claude", tr)))
}

func claudeTransport() publish.Transport {
	a, _ := publish.Lookup("claude")
	return a.Transport()
}

func setSend(t *testing.T, send func(context.Context, domain.PublishTarget, string) domain.PublishOutcome) {
	tr := claudeTransport()
	tr.Send = send
	useTransport(t, tr)
}

func setRetired(t *testing.T, retired func(context.Context, domain.PublishQueue) bool) {
	tr := claudeTransport()
	tr.Retired = retired
	useTransport(t, tr)
}

type markedNotice struct{ publish.Adapter }

func (markedNotice) Notice(ids []int64) (string, bool) { return fmt.Sprint("marked ", ids), true }

func TestPublisherRendersTheNoticeThroughTheAdapter(t *testing.T) {
	f := newPublishTestFixture(t)
	f.ask(t, 11)
	lines := make(chan string, 1)
	registry := publish.Registry{"claude": markedNotice{publish.Shipped()["claude"]}}.WithTransport("claude",
		publish.Transport{Send: func(_ context.Context, _ publish.Target, line string) publish.Outcome {
			lines <- line
			return publish.Submitted
		}})
	t.Cleanup(publish.Swap(registry))
	p := &publisher{db: f.db}
	t.Cleanup(p.workers.Wait)
	tickPublisher(t, p, f.now, true)
	if line := awaitPublishTest(t, lines); line != "marked [11]" {
		t.Fatalf("sent %q, want the adapter's own text", line)
	}
}

func TestPublisherWithoutASenderAnyMoreRecordsNoSubmission(t *testing.T) {
	f := newPublishTestFixture(t)
	f.ask(t, 11)
	id := f.reserve(t, false)
	t.Cleanup(publish.Swap(publish.Shipped().WithTransport("claude", publish.Transport{}))) // the sender is gone after the reservation
	p := &publisher{db: f.db}
	p.submit(t.Context(), id)
	f.requireAttempt(t, id, domain.PublishNotSubmitted)
}

func claudeNotice(ids []int64) string {
	line, _ := publish.Shipped()["claude"].Notice(ids)
	return line
}

// fixtureAdapter is an adapter this build does not ship: its address is the
// route's thread and its notice is "fixture <ids>".
type fixtureAdapter struct{ transport publish.Transport }

func (fixtureAdapter) Resolve(r publish.Route) (string, string) {
	if r.ThreadID == "" {
		return "", "no thread"
	}
	return r.ThreadID, ""
}
func (fixtureAdapter) ModeEligible(string) bool        { return true }
func (fixtureAdapter) AcceptsSocket(string) bool       { return false }
func (fixtureAdapter) SessionThread(ref string) string { return ref }
func (fixtureAdapter) Notice(ids []int64) (string, bool) {
	return "fixture " + notice.IDList(ids), true
}
func (fixtureAdapter) Echo(prompt string) (string, bool) {
	return strings.CutPrefix(prompt, "fixture ")
}
func (a fixtureAdapter) Transport() publish.Transport { return a.transport }

func countPublish(t *testing.T, f *publishTestFixture, table string) int {
	t.Helper()
	var n int
	f.update(t, func(tx store.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM `+table).Scan(&n)
	})
	return n
}

// Astra #426: another adapter's sender must not make a Claude route
// without one reserve, fail and suspend.
func TestPublisherReservesOnlyForAnAdapterThatSends(t *testing.T) {
	f := newPublishTestFixture(t)
	sent := make(chan string, 4)
	t.Cleanup(publish.Swap(publish.Shipped().WithTransport("claude", publish.Transport{}).With("fixture", fixtureAdapter{publish.Transport{
		Send: func(_ context.Context, _ publish.Target, line string) publish.Outcome {
			sent <- line
			return publish.Submitted
		},
	}})))
	p := &publisher{db: f.db}
	t.Cleanup(p.workers.Wait)
	for i, id := range []int64{11, 12, 13} {
		f.ask(t, id)
		tickPublisher(t, p, f.now.Add(time.Duration(i)*2*time.Second), i == 0)
	}
	p.workers.Wait()
	requireNone(t, sent)
	if n := countPublish(t, f, "publish_attempt"); n != 0 {
		t.Fatalf("%d attempts reserved for a route whose adapter cannot send", n)
	}
	if n := countPublish(t, f, "publish_suspension"); n != 0 {
		t.Fatalf("%d suspensions", n)
	}
	var state store.PublishState
	f.update(t, func(tx store.Tx) (err error) {
		state, _, err = store.ReadPublishState(t.Context(), tx, 2, f.now)
		return err
	})
	if state.State != "candidate" || state.Reason != "no sender for claude" {
		t.Fatalf("status %+v, want the same answer selection gave", state)
	}
}

// A route the transport has not proven reserves nothing; the adapter's
// proof is asked for first and, once recorded, the route publishes.
func TestPublisherProvesARouteBeforeReserving(t *testing.T) {
	for _, proves := range []bool{true, false} {
		t.Run(fmt.Sprintf("proves=%t", proves), func(t *testing.T) {
			f := newPublishTestFixture(t)
			f.provable(t)
			proofs := make(chan publish.Probe, 4)
			sent := make(chan string, 4)
			useTransport(t, publish.Transport{
				Prove: func(_ context.Context, probe publish.Probe) (publish.Proven, bool) {
					proofs <- probe
					return publish.Proven{Queue: publishTestTarget.Queue, StartedAtMS: 1234}, proves
				},
				Send: func(_ context.Context, _ publish.Target, line string) publish.Outcome {
					sent <- line
					return publish.Submitted
				},
			})
			p := &publisher{db: f.db}
			t.Cleanup(p.workers.Wait)
			tickPublisher(t, p, f.now, true)
			p.workers.Wait()
			if got := awaitPublishTest(t, proofs); got.Adapter != "claude" || got.Address != publishTestTarget.Address ||
				got.LaunchRef != "pid-2345" || got.StartedHookAtMS != 2000 {
				t.Fatalf("proved %+v", got)
			}
			if n := countPublish(t, f, "publish_attempt"); n != 0 {
				t.Fatalf("%d attempts before a proof", n)
			}
			tickPublisher(t, p, f.now.Add(500*time.Millisecond), true) // activates a recorded proof
			p.workers.Wait()
			f.ask(t, 11)
			tickPublisher(t, p, f.now.Add(time.Second), true)
			p.workers.Wait()
			if !proves {
				requireNone(t, sent)
				requireNone(t, proofs) // a failed proof waits publishProveRetry
				if n := countPublish(t, f, "publish_route_proof"); n != 0 {
					t.Fatalf("a failed proof was recorded")
				}
				return
			}
			if line := awaitPublishTest(t, sent); line != claudeNotice([]int64{11}) {
				t.Fatalf("sent %q", line)
			}
			requireNone(t, proofs)
		})
	}
}

func requireNone[T any](t *testing.T, ch <-chan T) {
	t.Helper()
	select {
	case value := <-ch:
		t.Fatalf("unexpected %+v", value)
	default:
	}
}

// provable makes the fixture's route unproven and its launch the process
// publishTestTarget.Queue names, so a transport can prove it.
func (f *publishTestFixture) provable(t *testing.T) {
	t.Helper()
	f.exec(t, `DELETE FROM publish_route_proof;
		UPDATE adapter_launch SET launch_ref = 'pid-2345', started_hook_at_ms = 2000;
		UPDATE publish_route SET launch_ref = 'pid-2345'`)
}

// Astra #433: two launches whose routes name one listener; the listener is
// proven only for the launch it is.
func TestAListenerIsProvenOnlyForItsOwnLaunch(t *testing.T) {
	f := newPublishTestFixture(t)
	f.provable(t)
	f.exec(t, `INSERT INTO principal(id, kind, vendor, created_at_ms) VALUES (3, 'session', 'claude', 1);
		INSERT INTO binding(id, principal_id, channel_id, alias, joined_at_ms) VALUES (3, 3, 1, 'other', 1);
		INSERT INTO adapter_launch(principal_id, launch_ref, profile, verified, started_hook_at_ms)
			VALUES (3, 'pid-2346', '{"vendor_version":"2.1.0","platform":"darwin-arm64","mode":"default","manifest_rev":"r1"}', 1, 2000);
		INSERT INTO publish_route(binding_id, principal_id, generation, adapter, session_ref, launch_ref,
			launch_verified, policy_epoch, mode, socket_path, watermark_message_id, state, registered_at_ms, updated_at_ms)
			VALUES (3, 3, 1, 'claude', 'session-3', 'pid-2346', 1, 1, 'tui', '/unused/claude.sock', 10, 'active', 1, 1)`)
	asked := make(chan string, 4)
	useTransport(t, publish.Transport{
		Prove: func(_ context.Context, probe publish.Probe) (publish.Proven, bool) {
			asked <- probe.LaunchRef
			return publish.Proven{Queue: publishTestTarget.Queue, StartedAtMS: 1234}, true // the one listener on the socket
		},
		Send: func(context.Context, publish.Target, string) publish.Outcome { return publish.Submitted },
	})
	p := &publisher{db: f.db}
	t.Cleanup(p.workers.Wait)
	tickPublisher(t, p, f.now, true)
	p.workers.Wait()
	got := []string{awaitPublishTest(t, asked), awaitPublishTest(t, asked)}
	slices.Sort(got)
	if !slices.Equal(got, []string{"pid-2345", "pid-2346"}) {
		t.Fatalf("proofs asked for %v", got)
	}
	var proven []int64
	f.update(t, func(tx store.Tx) error {
		rows, err := tx.QueryContext(t.Context(), `SELECT binding_id FROM publish_route_proof ORDER BY binding_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return err
			}
			proven = append(proven, id)
		}
		return rows.Err()
	})
	if !slices.Equal(proven, []int64{2}) {
		t.Fatalf("proven bindings %v, want only the listener's own launch", proven)
	}
}
