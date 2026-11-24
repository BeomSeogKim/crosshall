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

// A resume that is not submitted keeps the member due (it spends nothing);
// a member that answered meanwhile, or one past the window, is dropped.
func TestPublisherKeepsAResumeDueUntilSubmitted(t *testing.T) {
	f := newPublishTestFixture(t)
	f.ask(t, 10)
	lines := make(chan string, 4)
	setSend(t, func(_ context.Context, _ domain.PublishTarget, line string) domain.PublishOutcome {
		lines <- line
		return domain.PublishNotSubmitted
	})
	p := &publisher{db: f.db}
	t.Cleanup(p.workers.Wait)
	tickPublisher(t, p, f.now, true)
	p.workers.Wait()
	if line := awaitPublishTest(t, lines); line != claudeResume(10) {
		t.Fatalf("sent %q", line)
	}
	due := func() bool {
		p.resumeMu.Lock()
		defer p.resumeMu.Unlock()
		_, ok := p.resume[2]
		return ok && len(p.resumeAttempts) == 0
	}
	if !due() {
		t.Fatal("a resume that was not submitted spent the member's one resume")
	}

	f.exec(t, `UPDATE obligation SET state = 'replied' WHERE message_id = 10`)
	tickPublisher(t, p, f.now.Add(time.Minute), false)
	if due() {
		t.Fatal("a member that owes nothing is still due")
	}

	f.exec(t, `UPDATE obligation SET state = 'open' WHERE message_id = 10`)
	p.resumeMu.Lock()
	p.resume[2] = f.now.Add(time.Minute)
	p.resumeMu.Unlock()
	tickPublisher(t, p, f.now.Add(2*time.Minute), false)
	if due() {
		t.Fatal("a member past the window is still due")
	}
	var events int
	f.update(t, func(tx store.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM event WHERE kind = 'publish_resume'`).Scan(&events)
	})
	if events != 1 {
		t.Fatalf("%d publish_resume events, want one per reservation", events)
	}
}
