package cli

import (
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
)

const (
	waitUsage     = "usage: crosshall wait [--any | --work-only] [--existing] [--timeout <positive duration, e.g. 30s>]"
	waitFirstPoll = 100 * time.Millisecond
	waitMaxPoll   = time.Second
)

type waitResult struct {
	domain.InboxResult
	Reason string `json:"reason,omitempty"`
}

func runWait(ctx context.Context, env Env, opts options, args []string, stdout, stderr io.Writer) core.ExitCode {
	o := outputs{json: opts.json, stdout: stdout, stderr: stderr}
	var cmd domain.Wait
	var timeout time.Duration
	for i := 0; i < len(args); i++ {
		if args[i] == "--any" && !cmd.Any {
			cmd.Any = true
			continue
		}
		if args[i] == "--existing" && !cmd.Existing {
			cmd.Existing = true
			continue
		}
		if args[i] == "--work-only" && !cmd.WorkOnly {
			cmd.WorkOnly = true
			continue
		}
		value, last, found, err := flagValue(args, i, "--timeout")
		if !found || err != nil || timeout != 0 {
			return o.usage(waitUsage)
		}
		timeout, err = time.ParseDuration(value)
		if err != nil || timeout <= 0 {
			return o.usage(waitUsage)
		}
		i = last
	}
	if cmd.Any && cmd.WorkOnly {
		return o.usage(waitUsage)
	}
	if env.Transport == nil {
		return o.unavailableFor(env, cmd.Verb())
	}

	waitCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	end := func() core.ExitCode {
		if ctx.Err() != nil {
			return o.fail(core.ExitError, CodeLocalError, ctx.Err().Error(), "")
		}
		return o.fail(core.ExitWaitTimeout, core.CodeWaitTimeout, "no new messages before the wait timeout", "")
	}

	delay := waitFirstPoll
	for {
		if waitCtx.Err() != nil {
			return end()
		}
		result, err := env.Transport.Do(waitCtx, Call{Command: cmd})
		if err != nil || result.Status != core.ExitOK {
			if waitCtx.Err() != nil {
				return end()
			}
			if err != nil {
				return o.fail(core.ExitError, CodeLocalError, err.Error(), "")
			}
			if result.Status == core.ExitUnavailable {
				return o.fail(result.Status, result.Code, string(result.Code),
					"if the dispatcher is down, run crosshall owner start and retry crosshall wait")
			}
			return render(opts, cmd, result, stdout, stderr)
		}
		var snapshot domain.WaitSnapshot
		if err := json.Unmarshal(result.Data, &snapshot); err != nil || snapshot.Cursor.BindingID <= 0 {
			return o.fail(core.ExitError, CodeLocalError, "malformed wait snapshot", "")
		}
		if len(snapshot.Unread) != 0 {
			out := waitResult{InboxResult: snapshot.InboxResult}
			if cmd.Existing {
				out.Reason = "arrived"
				if cmd.Cursor == nil {
					out.Reason = "existing"
				}
			}
			arrival, err := domain.OK(out)
			if err != nil {
				return o.fail(core.ExitError, CodeLocalError, err.Error(), "")
			}
			// Observations neither acknowledge roster changes nor pick up items.
			return render(opts, cmd, arrival, stdout, stderr)
		}
		cmd.Cursor = &snapshot.Cursor
		// Start with a short interval, then back off to one snapshot per
		// second while the wait stays idle.
		timer := time.NewTimer(delay)
		select {
		case <-waitCtx.Done():
			timer.Stop()
			return end()
		case <-timer.C:
			delay = min(delay*2, waitMaxPoll)
		}
	}
}
