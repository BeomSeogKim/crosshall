package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/link"
)

func linkOwnerDenied(env Env, o outputs) (core.ExitCode, bool) {
	if env.OwnerGate != nil {
		if gate := env.OwnerGate(); gate != "" {
			return o.fail(core.ExitRefused, core.CodeOwnerOnly, gate, "rerun this command from your separate owner terminal"), true
		}
	}
	return core.ExitOK, false
}

func blockSpokeCommand(env Env, o outputs, verb string, args []string) (core.ExitCode, bool) {
	if env.LinkedTarget == nil {
		return core.ExitOK, false
	}
	switch verb {
	case "link", "unlink", "help", "--help", "-h", "version", "--version", "run":
		return core.ExitOK, false
	// The member commands the device forwards to the home (#400).
	case "join", "who", "inbox", "read", "ask", "reply", "note", "log", "leave", "focus", "retry", "wait":
		return core.ExitOK, false
	}
	if verb == "owner" && len(args) > 0 && args[0] == "help" {
		return core.ExitOK, false
	}
	target := env.LinkedTarget()
	if target == "" {
		return core.ExitOK, false
	}
	if verb == "owner" {
		return o.fail(core.ExitRefused, core.CodeOwnerOnly, "the owner seat is on the home device "+target,
			"ssh -- "+target+" crosshall owner help"), true
	}
	return o.fail(core.ExitUnavailable, core.CodeUnsent, "this device is linked to "+target+"; crosshall "+verb+" is not forwarded to the home yet",
		"ask the owner to run crosshall link status from their own shell"), true
}

func runLinkSpoke(ctx context.Context, env Env, o outputs, args []string, stdin io.Reader) core.ExitCode {
	if env.LinkSpoke == nil {
		return o.unavailableFor(env, "link")
	}
	var name string
	yes := false
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--yes":
			yes = true
		case "--name":
			if i+1 == len(args) || name != "" {
				return o.usage("link accepts one --name <device>")
			}
			i++
			name = args[i]
		default:
			return o.usage(fmt.Sprintf("unknown link option %q", args[i]))
		}
	}
	status, err := env.LinkSpoke(ctx, args[0], name, func(preview link.Preview) error {
		fmt.Fprintf(o.stderr, "Link this device to %s as %s. This build is %s, home is %s, common link version %d.\n",
			preview.Target, preview.Name, preview.LocalRelease, preview.HomeRelease, preview.Version)
		if preview.ClockOffsetMS != nil {
			fmt.Fprintf(o.stderr, "Estimated home clock offset: %+d ms.\n", *preview.ClockOffsetMS)
		}
		if yes {
			return nil
		}
		if stdin == nil {
			return errors.New("link needs confirmation; use --yes to confirm explicitly")
		}
		if env.FlushTerminalInput != nil {
			if err := env.FlushTerminalInput(); err != nil {
				return fmt.Errorf("cannot clear pending terminal input: %w", err)
			}
		}
		fmt.Fprint(o.stderr, "Link and enroll this device? [y/N] ")
		answer, _ := bufio.NewReader(io.LimitReader(stdin, 64)).ReadString('\n')
		answer = strings.TrimSpace(answer)
		switch strings.ToLower(answer) {
		case "y", "yes":
			return nil
		}
		return fmt.Errorf("link canceled (answer: %q)", answer)
	})
	if err != nil {
		return o.fail(core.ExitError, CodeLocalError, err.Error(), "")
	}
	return printLinkStatus(o, status)
}

func runLinkStatus(env Env, o outputs) core.ExitCode {
	if env.LinkStatus == nil {
		return o.unavailableFor(env, "link status")
	}
	status, err := env.LinkStatus()
	if err != nil {
		return o.fail(core.ExitError, CodeLocalError, err.Error(), "")
	}
	return printLinkStatus(o, status)
}

func runUnlink(ctx context.Context, env Env, o outputs, args []string) core.ExitCode {
	if code, denied := linkOwnerDenied(env, o); denied {
		return code
	}
	if len(args) != 0 {
		return o.usage("unlink takes no arguments")
	}
	if env.UnlinkSpoke == nil {
		return o.unavailableFor(env, "unlink")
	}
	if err := env.UnlinkSpoke(ctx); err != nil {
		return o.fail(core.ExitError, CodeLocalError, err.Error(), "")
	}
	return printLinkStatus(o, link.Status{State: "unlinked"})
}

func printLinkStatus(o outputs, status link.Status) core.ExitCode {
	if o.json {
		if err := json.NewEncoder(o.stdout).Encode(status); err != nil {
			return o.fail(core.ExitError, CodeLocalError, err.Error(), "")
		}
		return core.ExitOK
	}
	if status.Target == "" {
		fmt.Fprintln(o.stdout, "This device is not linked.")
		return core.ExitOK
	}
	fmt.Fprintf(o.stdout, "Home %s (%s).\n", status.Target, status.State)
	if status.Version != 0 {
		fmt.Fprintf(o.stdout, "Link %d, home release %s.\n", status.Version, status.HomeRelease)
	}
	if status.ClockOffsetMS != nil {
		fmt.Fprintf(o.stdout, "Estimated home clock offset: %+d ms.\n", *status.ClockOffsetMS)
	}
	if status.Error != "" {
		fmt.Fprintln(o.stdout, status.Error)
	}
	return core.ExitOK
}
