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
	*domain.MemberInboxSummary
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

	probe, _ := env.Transport.(interface{ DispatcherRunning() bool })
	checkDispatcher := func() core.ExitCode {
		if probe != nil && !probe.DispatcherRunning() {
			return o.fail(core.ExitUnavailable, core.CodeDispatcherUnavailable, "dispatcher is unavailable",
				"run crosshall owner start, check crosshall inbox, then retry crosshall wait")
		}
		return core.ExitOK
	}
	waitCtx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		waitCtx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	var latest *domain.MemberInboxSummary
	var cursorFingerprint string
	end := func() core.ExitCode {
		if ctx.Err() != nil {
			return o.fail(core.ExitError, CodeLocalError, ctx.Err().Error(), "")
		}
		if status := checkDispatcher(); status != core.ExitOK {
			return status
		}
		result, err := domain.WaitTimeout(latest)
		if err != nil {
			return o.fail(core.ExitError, CodeLocalError, err.Error(), "")
		}
		return render(opts, cmd, result, stdout, stderr)
	}

	delay := waitFirstPoll
	waitForNextPoll := func() bool {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-waitCtx.Done():
			return false
		case <-timer.C:
			delay = min(delay*2, waitMaxPoll)
			return true
		}
	}
	for {
		if waitCtx.Err() != nil {
			return end()
		}
		if status := checkDispatcher(); status != core.ExitOK {
			return status
		}
		result, err := env.Transport.Do(waitCtx, Call{Command: cmd})
		if err != nil || result.Status != core.ExitOK {
			if waitCtx.Err() != nil {
				return end()
			}
			if status := checkDispatcher(); status != core.ExitOK {
				return status
			}
			if err != nil {
				return o.fail(core.ExitError, CodeLocalError, err.Error(), "")
			}
			if result.Status == core.ExitUnavailable {
				if probe == nil {
					return o.fail(result.Status, result.Code, string(result.Code),
						"if the dispatcher is down, run crosshall owner start and retry crosshall wait")
				}
				if !waitForNextPoll() {
					return end()
				}
				continue
			}
			return render(opts, cmd, result, stdout, stderr)
		}
		var snapshot domain.WaitSnapshot
		if err := json.Unmarshal(result.Data, &snapshot); err != nil || snapshot.Cursor.BindingID <= 0 {
			return o.fail(core.ExitError, CodeLocalError, "malformed wait snapshot", "")
		}
		latest = snapshot.MemberInboxSummary
		if snapshot.CursorFingerprint != "" && cursorFingerprint != "" && cursorFingerprint != snapshot.PriorCursorFingerprint {
			result, err := domain.WaitCursorInvalidated(latest)
			if err != nil {
				return o.fail(core.ExitError, CodeLocalError, err.Error(), "")
			}
			return render(opts, cmd, result, stdout, stderr)
		}
		if len(snapshot.Unread) != 0 {
			out := waitResult{InboxResult: snapshot.InboxResult, MemberInboxSummary: latest}
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
		cursorFingerprint = snapshot.CursorFingerprint
		// Start with a short interval, then back off to one snapshot per
		// second while the wait stays idle.
		if !waitForNextPoll() {
			return end()
		}
	}
}
