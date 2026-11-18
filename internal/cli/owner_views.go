package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/BeomSeogKim/crosshall/internal/core"
	"github.com/BeomSeogKim/crosshall/internal/domain"
)

func runOwnerViews(ctx context.Context, env Env, opts options, args []string, stdout, stderr io.Writer) core.ExitCode {
	o := outputs{json: opts.json, stdout: stdout, stderr: stderr}
	if args[0] == "doctor" {
		if len(args) != 1 {
			return o.usage("doctor takes no arguments")
		}
		if env.Doctor == nil {
			return o.unavailableFor(env, "owner doctor")
		}
		return renderDoctor(o, env.Doctor(ctx))
	}
	call, err := ownerViewCall(args)
	if err != nil {
		return o.usage(err.Error())
	}
	if _, changesDefault := call.Command.(domain.ChannelDefault); changesDefault {
		call.OpKey = opKey(env, opts)
	}
	if env.Start != nil {
		if err := env.Start(ctx); err != nil {
			return render(opts, call.Command, domain.Fail(core.ExitUnavailable, core.CodeUnsent,
				"dispatcher could not start: "+err.Error(), "crosshall owner start"), stdout, stderr)
		}
	}
	return send(ctx, env, opts, call, stdout, stderr)
}

func ownerViewCall(args []string) (Call, error) {
	call := Call{Owner: true}
	switch args[0] {
	case "who":
		if len(args) != 2 || args[1] != "--all" {
			return Call{}, fmt.Errorf("usage: crosshall owner who --all")
		}
		call.Command = domain.OwnerWho{}
	case "log":
		if len(args) != 2 || !domain.ValidName(strings.TrimPrefix(args[1], "#")) {
			return Call{}, fmt.Errorf("usage: crosshall owner log <channel>")
		}
		call.Command = domain.OwnerLog{Channel: strings.TrimPrefix(args[1], "#")}
	case "channel":
		if len(args) < 3 {
			return Call{}, fmt.Errorf("usage: crosshall owner channel default <channel> [--repo <directory>]")
		}
		channel := strings.TrimPrefix(args[2], "#")
		if !domain.ValidName(channel) {
			return Call{}, fmt.Errorf("invalid channel name %q", args[2])
		}
		var path string
		if len(args) > 3 {
			value, last, found, err := flagValue(args, 3, "--repo")
			if !found || err != nil || value == "" || last != len(args)-1 {
				return Call{}, fmt.Errorf("usage: crosshall owner channel default <channel> [--repo <directory>]")
			}
			path = value
		}
		root, err := repoRoot(path)
		if err != nil {
			return Call{}, err
		}
		call.Command = domain.ChannelDefault{Channel: channel, RepoRoot: root}
	}
	return call, nil
}

func renderOwnerViews(cmd domain.Command, data []byte, w io.Writer) error {
	switch cmd.(type) {
	case domain.OwnerWho:
		var r domain.OwnerWhoResult
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		if len(r.Channels) == 0 {
			fmt.Fprintln(w, "No channels.")
		}
		for _, ch := range r.Channels {
			fmt.Fprintf(w, "#%s", ch.Name)
			if ch.Topic != "" {
				fmt.Fprintf(w, " — %s", ch.Topic)
			}
			fmt.Fprintln(w)
			for _, member := range ch.Members {
				focus := member.Focus
				if focus == "" {
					focus = "focus unset"
				}
				fmt.Fprintf(w, "  @%s [binding %d] %s, %s, %s; %s, last command %s ago\n",
					member.Alias, member.BindingID, member.BindingState, member.Vendor, focus, member.Route, age(member.LastCommandAgoS))
			}
		}
	case domain.OwnerLog:
		var r domain.LogResult
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		// Owner history has a named-channel page, so the member continuation
		// command printed by renderLog would select a different scope.
		r.Next = nil
		renderLog(w, r, domain.DefaultLogLimit)
		if r.Earlier > 0 {
			fmt.Fprintf(w, "... %d earlier in #%s\n", r.Earlier, r.Channel)
		}
	case domain.ChannelDefault:
		var r domain.ChannelDefaultResult
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		fmt.Fprintf(w, "#%s is the default for %s.\n", r.Channel, r.RepoRoot)
	default:
		return fmt.Errorf("no owner display for %T", cmd)
	}
	return nil
}

type doctorCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

func renderDoctor(o outputs, r core.Result) core.ExitCode {
	var result struct {
		Checks []doctorCheck `json:"checks"`
	}
	if err := json.Unmarshal(r.Data, &result); err != nil || len(result.Checks) == 0 {
		return o.fail(core.ExitError, CodeLocalError, "no diagnostic checks were returned", "")
	}
	if o.json {
		fields := map[string]any{"v": 1, "ok": r.Status == core.ExitOK, "kind": "owner doctor", "checks": result.Checks}
		if r.Status != core.ExitOK {
			fields["code"], fields["message"], fields["hint"] = CodeLocalError, "one or more checks failed", ""
		}
		if err := json.NewEncoder(o.stdout).Encode(fields); err != nil {
			return core.ExitError
		}
		return r.Status
	}
	for _, check := range result.Checks {
		outcome := "FAIL"
		if check.OK {
			outcome = "PASS"
		}
		if _, err := fmt.Fprintf(o.stdout, "%s %s — %s\n", outcome, check.Name, check.Detail); err != nil {
			return core.ExitError
		}
	}
	return r.Status
}
