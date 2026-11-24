package dispatch

import (
	"context"
	"testing"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/publish"
	"github.com/BeomSeogKim/crosshall/internal/store"
)

func claudeResume(ids ...int64) string {
	line, _ := publish.Shipped()["claude"].ResumeNotice(ids)
	return line
}

// observeAll frees the lanes as a receipt would.
func (f *publishTestFixture) observeAll(t *testing.T) {
	t.Helper()
	f.exec(t, `UPDATE publish_attempt SET observed_at_ms = 1 WHERE observed_at_ms IS NULL AND state = 'submitted'`)
}

// A member that owes a request no notice carries gets one resume notice
// after a start, and no second one until the next start.
func TestPublisherResumesOwedWorkOncePerStart(t *testing.T) {
	f := newPublishTestFixture(t)
	f.ask(t, 10) // at the route's watermark: no notice carries it
	p, calls := f.publisher(t, nil)
	tickPublisher(t, p, f.now, true)
	p.workers.Wait()
	if call := awaitPublishTest(t, calls); call.line != claudeResume(10) || call.target != publishTestTarget {
		t.Fatalf("published %q to %+v, want the resume notice of 10", call.line, call.target)
	}
	f.requireAttempt(t, 1, domain.PublishSubmitted)
	f.observeAll(t)
	for i := 1; i <= 3; i++ {
		tickPublisher(t, p, f.now.Add(time.Duration(i)*time.Second), false)
		p.workers.Wait()
	}
	requireNoPublish(t, calls)

	// The next start resumes it again.
	tickPublisher(t, p, f.now.Add(time.Minute), true)
	p.workers.Wait()
	if call := awaitPublishTest(t, calls); call.line != claudeResume(10) {
		t.Fatalf("after a new start published %q", call.line)
	}
}

// A resume the transport refuses keeps the member due for one more try,
// resumeRetry later (Fable F1 on #342): a refusing transport gets two sends
// per start, not one a tick, and no suspension. A member that answered
// meanwhile, or one past the window, is dropped.
func TestPublisherRetriesARefusedResumeOnce(t *testing.T) {
	f := newPublishTestFixture(t)
	f.ask(t, 10)
	lines := make(chan string, 64)
	setSend(t, func(_ context.Context, _ domain.PublishTarget, line string) domain.PublishOutcome {
		lines <- line
		return domain.PublishNotSubmitted
	})
	p := &publisher{db: f.db}
	t.Cleanup(p.workers.Wait)
	due := func() bool {
		p.resumeMu.Lock()
		defer p.resumeMu.Unlock()
		_, ok := p.resume[2]
		return ok
	}
	// Every 250 ms for the first resumeRetry, then past it, then on.
	for at := time.Duration(0); at <= 2*resumeRetry; at += 250 * time.Millisecond {
		if at > time.Second && at < resumeRetry-time.Second {
			at = resumeRetry - time.Second
		}
		tickPublisher(t, p, f.now.Add(at), at == 0)
		p.workers.Wait()
	}
	var sent []string
	for len(lines) > 0 {
		sent = append(sent, <-lines)
	}
	if len(sent) != resumeTries || sent[0] != claudeResume(10) {
		t.Fatalf("a refusing transport got %d sends (%q), want %d", len(sent), sent, resumeTries)
	}
	if due() {
		t.Fatal("a member is still due after its last try")
	}
	if n := countPublish(t, f, "publish_attempt"); n != resumeTries {
		t.Fatalf("%d attempts, want %d", n, resumeTries)
	}
	if n := countPublish(t, f, "publish_suspension"); n != 0 {
		t.Fatalf("%d suspensions from the resume alone", n)
	}
	var events int
	f.update(t, func(tx store.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM event WHERE kind = 'publish_resume'`).Scan(&events)
	})
	if events != resumeTries {
		t.Fatalf("%d publish_resume events, want one per reservation", events)
	}

	// A new start: due again; answering drops it, and so does the window.
	tickPublisher(t, p, f.now.Add(time.Hour), true)
	p.workers.Wait()
	f.exec(t, `UPDATE obligation SET state = 'replied' WHERE message_id = 10`)
	p.resumeMu.Lock()
	p.resumeNext[2] = time.Time{}
	p.resumeMu.Unlock()
	tickPublisher(t, p, f.now.Add(time.Hour+time.Minute), false)
	if due() {
		t.Fatal("a member that owes nothing is still due")
	}
	f.exec(t, `UPDATE obligation SET state = 'open' WHERE message_id = 10`)
	p.resumeMu.Lock()
	p.resume[2] = f.now.Add(time.Hour + time.Minute)
	p.resumeMu.Unlock()
	tickPublisher(t, p, f.now.Add(time.Hour+2*time.Minute), false)
	if due() {
		t.Fatal("a member past the window is still due")
	}
}
