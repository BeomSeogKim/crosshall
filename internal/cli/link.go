package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
	"github.com/BeomSeogKim/crosshall/internal/link"
)

// linkCallBudget bounds each call a link command makes to the dispatcher.
const linkCallBudget = 3 * time.Second

// Link frame types outside the handshake.
const (
	linkEnrolled = "enrolled"
	linkPing     = "ping"
	linkPong     = "pong"
	// A member command forwarded by the device, and the hall's answer.
	linkCallFrame   = "call"
	linkResultFrame = "result"
)

// runLink runs the home side of a link (#400). A spoke starts these over
// the user's own ssh: `link enroll` once, `link serve` for each connection.
// Both stay behind CROSSHALL_EXPERIMENTAL_LINK until the release gate
// covers two devices.
func runLink(ctx context.Context, env Env, opts options, args []string, stdin io.Reader, stdout, stderr io.Writer) core.ExitCode {
	// Stdout carries link frames only, so every message goes to stderr,
	// with --json too.
	o := outputs{json: opts.json, stdout: stderr, stderr: stderr}
	if !env.ExperimentalLink {
		return o.fail(core.ExitUsage, CodeUsage, "linking devices is experimental",
			"set CROSSHALL_EXPERIMENTAL_LINK=1 to try it")
	}
	if env.OwnerGate != nil {
		if gate := env.OwnerGate(); gate != "" {
			return render(opts, nil, domain.Refuse(core.CodeOwnerOnly, gate,
				"run crosshall link from your own shell"), stderr, stderr)
		}
	}
	if env.Transport == nil {
		return o.unavailableFor(env, "link")
	}
	switch {
	case len(args) == 3 && args[0] == "enroll" && args[1] == "--name":
		return linkEnroll(ctx, env, opts, args[2], stdin, stdout, stderr)
	case len(args) == 1 && args[0] == "serve":
		// The stream carries frames only: every message goes to stderr.
		return linkServe(ctx, env, stdin, stdout, o)
	}
	return o.usage("usage: crosshall link enroll --name <device> (the secret on stdin) | crosshall link serve")
}

// ownerCall runs one owner command with its own deadline, starting the
// dispatcher if needed.
func ownerCall(ctx context.Context, env Env, call Call) (core.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, linkCallBudget)
	defer cancel()
	if env.Start != nil {
		if err := env.Start(ctx); err != nil {
			return core.Result{}, fmt.Errorf("dispatcher could not start: %w", err)
		}
	}
	call.Owner = true
	return env.Transport.Do(ctx, call)
}

func ownerData[T any](ctx context.Context, env Env, call Call) (T, error) {
	var v T
	r, err := ownerCall(ctx, env, call)
	if err != nil {
		return v, err
	}
	if r.Status != core.ExitOK {
		var msg struct{ Message string }
		_ = json.Unmarshal(r.Data, &msg)
		return v, fmt.Errorf("%s: %s", call.Command.Verb(), msg.Message)
	}
	return v, json.Unmarshal(r.Data, &v)
}

// linkEnroll reads the device secret as one hex line on stdin, so it never
// shows in a process list, and writes the hall and device as one frame.
func linkEnroll(ctx context.Context, env Env, opts options, name string, stdin io.Reader, stdout, stderr io.Writer) core.ExitCode {
	o := outputs{stdout: stderr, stderr: stderr}
	if stdin == nil {
		return o.usage("link enroll reads the device secret on stdin")
	}
	line, err := bufio.NewReader(io.LimitReader(stdin, 4096)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return o.fail(core.ExitError, CodeLocalError, err.Error(), "")
	}
	cmd := domain.LinkEnroll{Name: name, Secret: strings.TrimSpace(line)}
	r, err := ownerCall(ctx, env, Call{Command: cmd, OpKey: opKey(env, opts)})
	if err != nil {
		return o.fail(core.ExitUnavailable, core.CodeUnsent, err.Error(), "crosshall owner doctor")
	}
	if r.Status != core.ExitOK {
		return render(opts, cmd, r, stderr, stderr)
	}
	// One frame, so the spoke fails closed on anything else the remote
	// shell printed, as the link itself does.
	if err := link.NewWriter(stdout).Write(linkEnrolled, r.Data); err != nil {
		return o.fail(core.ExitError, CodeLocalError, err.Error(), "")
	}
	return core.ExitOK
}

// linkServe runs the handshake as the home and then answers the spoke
// until it hangs up. It asks the dispatcher for the hall, the home's proof
// and the check of the spoke's, so no device secret reaches this process.
func linkServe(ctx context.Context, env Env, stdin io.Reader, stdout io.Writer, o outputs) core.ExitCode {
	if stdin == nil {
		return o.fail(core.ExitUsage, CodeUsage, "link serve speaks on stdin and stdout", "")
	}
	hall, err := ownerData[domain.LinkHallResult](ctx, env, Call{Command: domain.LinkHall{}})
	if err != nil {
		return o.fail(core.ExitUnavailable, core.CodeUnsent, err.Error(), "crosshall owner doctor")
	}
	r, w := link.NewReader(stdin), link.NewWriter(stdout)
	// The credential generation the link authenticated under; every call
	// carries it, and the dispatcher refuses it once the device enrolls
	// again.
	var credentialGen int64
	home := link.Home{Hall: hall.Hall, Release: env.Version, Link: link.Supported(),
		Prove: func(device, homeNonce, spokeNonce string) (bool, string, error) {
			v, err := ownerData[domain.LinkProveResult](ctx, env,
				Call{Command: domain.LinkProve{Device: device, HomeNonce: homeNonce, SpokeNonce: spokeNonce}})
			return v.Known, v.Proof, err
		},
		Verify: func(device, homeNonce, spokeNonce, proof string) (bool, error) {
			v, err := ownerData[domain.LinkVerifyResult](ctx, env,
				Call{Command: domain.LinkVerify{Device: device, HomeNonce: homeNonce, SpokeNonce: spokeNonce, Proof: proof}})
			credentialGen = v.CredentialGen
			return v.OK, err
		}}
	session, err := home.Accept(r, w)
	if err != nil {
		return o.fail(core.ExitError, CodeLocalError, err.Error(), "")
	}
	for {
		f, err := r.Read()
		if errors.Is(err, io.EOF) {
			return core.ExitOK
		}
		if err != nil {
			return o.fail(core.ExitError, CodeLocalError, err.Error(), "")
		}
		switch f.Type {
		case linkPing:
			if err := w.Write(linkPong, f.Body); err != nil {
				return o.fail(core.ExitError, CodeLocalError, err.Error(), "")
			}
		case linkCallFrame:
			var call domain.LinkCall
			if err := json.Unmarshal(f.Body, &call); err != nil {
				return o.fail(core.ExitError, CodeLocalError, "link: malformed call", "")
			}
			// The device and credential come from this link's
			// authentication, never from the device's frame.
			call.Device, call.CredentialGen = session.Device, credentialGen
			result, err := ownerCall(ctx, env, Call{Command: call})
			if err != nil {
				return o.fail(core.ExitUnavailable, core.CodeUnsent, err.Error(), "crosshall owner doctor")
			}
			if err := w.Write(linkResultFrame, result); err != nil {
				return o.fail(core.ExitError, CodeLocalError, err.Error(), "")
			}
			if result.Code == domain.CodeLinkStale {
				return o.fail(core.ExitRefused, domain.CodeLinkStale, "the device enrolled again; this link is closed", "")
			}
		default:
			return o.fail(core.ExitError, CodeLocalError,
				fmt.Sprintf("link %d has no %q frame", session.Link, link.Printable(f.Type)), "")
		}
	}
}
