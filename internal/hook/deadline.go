package hook

import (
	"context"
	"os"
	"time"
)

const counterAllowance = 5 * time.Millisecond

func runTimed(entry time.Time, prepare func() (*os.Root, *hookCounter), dispatch func(context.Context, *os.Root) string) {
	end := entry.Add(Budget)
	workEnd := end.Add(-counterAllowance)
	ctx, cancel := context.WithDeadline(context.Background(), workEnd)
	defer cancel()
	ready := make(chan *hookCounter, 1)
	done := make(chan string, 1)
	go func() {
		defer close(ready)
		defer close(done)
		if !time.Now().Before(end) {
			return
		}
		rv, counter := prepare()
		if rv == nil {
			return
		}
		defer rv.Close()
		if !time.Now().Before(end) {
			if counter != nil {
				counter.file.Close()
			}
			return
		}
		ready <- counter
		if !hookExpired(ctx) {
			done <- dispatch(ctx, rv)
		}
	}()
	// Preparation, including path lookup, never gets extra time. If both
	// signals are ready after a suspension, take the pre-opened counter.
	timer := time.NewTimer(time.Until(end))
	defer timer.Stop()
	var counter *hookCounter
	select {
	case counter = <-ready:
	case <-timer.C:
		select {
		case counter = <-ready:
		default:
			return
		}
	}
	outcome := "deadline"
	if time.Now().Before(workEnd) {
		select {
		case outcome = <-done:
		case <-ctx.Done():
		}
	}
	cancel()
	finishCounter(entry, outcome, counter)
}

// A late wake can report the missed deadline only on a pre-opened file.
// An append started on time still shares the original absolute deadline.
func finishCounter(entry time.Time, outcome string, counter *hookCounter) {
	if counter == nil {
		return
	}
	now := time.Now()
	end := entry.Add(Budget)
	if !now.Before(end.Add(-counterAllowance)) {
		outcome = "deadline"
	}
	if !now.Before(end) {
		end = now.Add(counterAllowance)
	}
	timer := time.NewTimer(time.Until(end))
	defer timer.Stop()
	written := make(chan struct{})
	go func() {
		defer close(written)
		defer counter.file.Close()
		if time.Now().Before(end) {
			counter.append(outcome, entry)
		}
	}()
	select {
	case <-written:
	case <-timer.C:
	}
}

func hookExpired(ctx context.Context) bool {
	end, ok := ctx.Deadline()
	return ctx.Err() != nil || (ok && !time.Now().Before(end))
}
