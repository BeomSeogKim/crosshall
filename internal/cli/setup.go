package cli

import (
	"io"
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
}

func runSetup(env Env, opts options, action string, args []string, stdout, stderr io.Writer) core.ExitCode {
	o := outputs{json: opts.json, stdout: stdout, stderr: stderr}
	if env.OwnerGate != nil {
		if msg := env.OwnerGate(); msg != "" {
			return render(opts, nil, domain.Refuse(core.CodeOwnerOnly, msg,
				"run adapter installation from your own shell"), stdout, stderr)
		}
	}
	req := SetupRequest{Action: action, JSON: opts.json, Input: env.Stdin, Output: stdout, Prompt: stderr}
	switch {
	case len(args) == 0:
	case len(args) == 1 && args[0] == "--dry-run":
		req.DryRun = true
	case len(args) == 1 && args[0] == "off" && action == "setup":
		req.Action = "off"
	default:
		return o.usage("usage: setup [--dry-run] | setup off | uninstall [--dry-run]")
	}
	if env.Setup == nil {
		return o.unavailableFor(env, action)
	}
	if req.Input == nil {
		req.Input = strings.NewReader("")
	}
	if err := env.Setup(req); err != nil {
		return o.fail(core.ExitError, CodeLocalError, err.Error(), "")
	}
	return core.ExitOK
}
