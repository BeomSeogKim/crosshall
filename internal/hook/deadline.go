package hook

import (
	"context"
	"io"
	"os"
	"sync/atomic"
	"time"
)

const counterAllowance = 5 * time.Millisecond

// A callback may write one vendor response. The supervisor seals stdout
// before accounting, even if the worker was paused just before Write.
type hookOutput struct {
	dst   io.Writer
	state atomic.Uint32
}

const (
	outputIdle uint32 = iota
	outputWriting
	outputWritten
	outputSealed
)

func (o *hookOutput) Write(p []byte) (int, error) {
	if !o.state.CompareAndSwap(outputIdle, outputWriting) {
		return 0, context.DeadlineExceeded
	}
	n, err := o.dst.Write(p)
	if !o.state.CompareAndSwap(outputWriting, outputWritten) && err == nil {
		return n, context.DeadlineExceeded
	}
	return n, err
}

func runTimed(entry time.Time, budget time.Duration, stdout io.Writer, prepare func() (*os.Root, *hookCounter), dispatch func(context.Context, *os.Root, io.Writer) string) bool {
	output := &hookOutput{dst: stdout}
	end := entry.Add(budget)
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
			done <- dispatch(ctx, rv, output)
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
			return output.state.Swap(outputSealed) != outputIdle
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
	return finishCounter(entry, budget, outcome, counter, output)
}

// A late wake can report the missed deadline only on a pre-opened file.
// An append started on time still shares the original absolute deadline.
func finishCounter(entry time.Time, budget time.Duration, outcome string, counter *hookCounter, output *hookOutput) bool {
	state := output.state.Swap(outputSealed)
	if counter == nil {
		return state != outputIdle
	}
	if state == outputWriting {
		// The outstanding write cannot be recalled. Return to the vendor
		// immediately instead of keeping the process alive for accounting.
		go counter.file.Close()
		return true
	}
	now := time.Now()
	end := entry.Add(budget)
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
	return state != outputIdle
}

func hookExpired(ctx context.Context) bool {
	end, ok := ctx.Deadline()
	return ctx.Err() != nil || (ok && !time.Now().Before(end))
}
