package cli

import (
	"bufio"
	"context"
	"errors"
	"io"
	"slices"
	"strings"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
)

type SetupRequest struct {
	Action string
	DryRun bool
	JSON   bool
	Input  io.Reader
	Output io.Writer
	Prompt io.Writer
	// Adapters names the vendors a setup or uninstall is limited to.
	Adapters []string
	// Apply installs or removes hooks without reading input.
	Apply bool
	// Offer is the installer's private form: ask only when eligible.
	Offer bool
	// Terminal reports that standard input and output are a terminal.
	Terminal       bool
	PathConsent    func(bool) error
	OfferTelemetry func(*bufio.Reader) error
	// Installed is told that at least one agent's hooks were installed.
	Installed func()
}

const setupUsage = "usage: setup [--dry-run] [claude|codex|antigravity ...] | setup <names> --apply | setup status | setup off | uninstall [--dry-run|--apply] [names]"

// scriptedForm is printed when an unnamed setup cannot ask questions.
const scriptedForm = `setup asks for confirmation in a terminal. Nothing was changed.
To install from a script, name the agents and add --apply:
  crosshall setup claude codex --apply
To preview first:
  crosshall setup --dry-run claude codex
Answers piped to setup, as in v0.1.0-rc.6, are no longer read.`

func runSetup(ctx context.Context, env Env, opts options, action string, args []string, stdout, stderr io.Writer) core.ExitCode {
	o := outputs{json: opts.json, stdout: stdout, stderr: stderr}
	if env.OwnerGate != nil {
		if msg := env.OwnerGate(); msg != "" {
			if action == "setup" && slices.Equal(args, []string{"--offer"}) {
				return core.ExitOK // the installer's offer stays silent for an agent
			}
			return render(opts, nil, domain.Refuse(core.CodeOwnerOnly, msg,
				"run adapter installation from your own shell"), stdout, stderr)
		}
	}
	req, ok := parseSetup(action, args, env.SetupAdapters)
	if !ok {
		return o.usage(setupUsage)
	}
	req.JSON, req.Input, req.Output, req.Prompt = opts.json, env.Stdin, stdout, stderr
	req.Terminal = env.SetupTerminal != nil && env.SetupTerminal()
	if req.Action == "setup" && req.Apply && env.BeginSetupApply != nil {
		var stop func()
		ctx, stop = env.BeginSetupApply(ctx)
		defer stop()
	}
	if action == "setup" && !req.DryRun {
		if err := applyTelemetryEnvironment(env, stderr); err != nil {
			return o.fail(core.ExitError, CodeLocalError, err.Error(), "")
		}
	}
	if req.Offer && (!req.Terminal || env.Setup == nil) {
		return core.ExitOK // the installer's offer: silent when it cannot ask
	}
	if req.Action == "setup" && !req.DryRun && !req.Apply && !req.Offer && len(req.Adapters) == 0 && !req.Terminal {
		return o.fail(core.ExitUsage, CodeUsage, scriptedForm, "crosshall setup claude codex --apply")
	}
	if env.Setup == nil {
		return o.unavailableFor(env, action)
	}
	if req.Input == nil {
		req.Input = strings.NewReader("")
	}
	if req.Action == "setup" && req.Terminal && !req.DryRun && !req.Apply && !req.JSON && env.TelemetryEndpoint != "" {
		source := "owner-prompt"
		if req.Offer {
			source = "installer"
		}
		req.OfferTelemetry = func(input *bufio.Reader) error {
			_, err := offerTelemetry(env, opts, input, stdout, source)
			return err
		}
	}
	if err := context.Cause(ctx); err != nil {
		return o.fail(core.ExitError, CodeLocalError, err.Error(), "")
	}
	if err := env.Setup(ctx, req); err != nil {
		var result interface{ Reported() bool }
		if errors.As(err, &result) && result.Reported() {
			return core.ExitError
		}
		if cause := context.Cause(ctx); cause != nil {
			err = cause
		}
		return o.fail(core.ExitError, CodeLocalError, err.Error(), "")
	}
	return core.ExitOK
}

// parseSetup reads the setup and uninstall forms. Vendor names must be
// shipped hook adapters, each named once.
func parseSetup(action string, args, adapters []string) (SetupRequest, bool) {
	req := SetupRequest{Action: action}
	if action == "setup" && len(args) == 1 {
		switch args[0] {
		case "off", "status":
			req.Action = args[0]
			return req, true
		case "--offer":
			req.Offer = true
			return req, true
		}
	}
	for _, arg := range args {
		if arg == "antigravity" {
			arg = "agy"
		}
		switch {
		case arg == "--dry-run" && !req.DryRun:
			req.DryRun = true
		case arg == "--apply" && !req.Apply:
			req.Apply = true
		case slices.Contains(adapters, arg) && !slices.Contains(req.Adapters, arg):
			req.Adapters = append(req.Adapters, arg)
		default:
			return req, false
		}
	}
	if req.Apply && (req.DryRun || action == "setup" && len(req.Adapters) == 0) {
		return req, false
	}
	return req, true
}
